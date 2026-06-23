package main

import (
	"context"
	crand "crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	mysql "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore
)

// Step17: users はほぼ不変（account_name/authority は固定、del_flg は ban/initialize でのみ変化）
// なので id→User をプロセス内キャッシュし、makePosts/getSessionUser の毎リク users SELECT と
// その行スキャン(scanAll)を排除する。無効化: /initialize で全クリア、ban で該当idを削除（次回DB再読込）。
var (
	userCacheMu sync.RWMutex
	userCache   = make(map[int]User)
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
	// Step14: テンプレレンダリング時の reflect.Value.Call を排除するため、
	// 旧 {{imageURL .}} / {{.CreatedAt.Format ...}} を事前計算した素の文字列フィールドに置換。
	ImageURL     string
	CreatedAtFmt string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

var memcacheClient *memcache.Client

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient = memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize(ctx context.Context) {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.ExecContext(ctx, sql)
	}
}

func tryLogin(ctx context.Context, accountName, password string) *User {
	u := User{}
	err := db.GetContext(ctx, &u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(ctx, u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// 旧実装はopensslを外部プロセスで起動していたが、Goのcrypto/sha512で同じ16進SHA-512を計算する。
// openssl dgst -sha512 の出力（小文字hex）と同一なので既存passhashと互換。
func digest(ctx context.Context, src string) string {
	sum := sha512.Sum512([]byte(src))
	return hex.EncodeToString(sum[:])
}

func calculateSalt(ctx context.Context, accountName string) string {
	return digest(ctx, accountName)
}

func calculatePasshash(ctx context.Context, accountName, password string) string {
	return digest(ctx, password+":"+calculateSalt(ctx, accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	ctx := r.Context()
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}
	var id int
	switch v := uid.(type) {
	case int:
		id = v
	case int64:
		id = int(v)
	default:
		return User{}
	}

	// Step17: userCache 経由（毎リクの SELECT users WHERE id=? を排除）。
	u, _ := getUserByID(ctx, id)
	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

// selectUsersInto は id IN (ids) のユーザーを取得して dest マップへ詰める（重複idは呼び出し側で除去済み前提）。
// Step17: プロセス内 userCache を一次参照し、未キャッシュidのみDBから取得して充填する。
func selectUsersInto(ctx context.Context, dest map[int]User, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	var missing []int
	userCacheMu.RLock()
	for _, id := range ids {
		if u, ok := userCache[id]; ok {
			dest[id] = u
		} else {
			missing = append(missing, id)
		}
	}
	userCacheMu.RUnlock()
	if len(missing) == 0 {
		return nil
	}

	q, args, err := sqlx.In("SELECT `id`, `account_name`, `del_flg`, `authority` FROM `users` WHERE `id` IN (?)", missing)
	if err != nil {
		return err
	}
	var users []User
	if err := db.SelectContext(ctx, &users, db.Rebind(q), args...); err != nil {
		return err
	}
	userCacheMu.Lock()
	for _, u := range users {
		userCache[u.ID] = u
		dest[u.ID] = u
	}
	userCacheMu.Unlock()
	return nil
}

// getUserByID は単一ユーザーを userCache 経由で取得する（getSessionUser用）。
func getUserByID(ctx context.Context, id int) (User, bool) {
	userCacheMu.RLock()
	u, ok := userCache[id]
	userCacheMu.RUnlock()
	if ok {
		return u, true
	}
	var user User
	if err := db.GetContext(ctx, &user, "SELECT `id`, `account_name`, `del_flg`, `authority` FROM `users` WHERE `id` = ?", id); err != nil {
		return User{}, false
	}
	userCacheMu.Lock()
	userCache[user.ID] = user
	userCacheMu.Unlock()
	return user, true
}

func makePosts(ctx context.Context, results []Post, csrfToken string, allComments bool) ([]Post, error) {
	if len(results) == 0 {
		return []Post{}, nil
	}

	// 投稿者のユーザー情報だけを取得（旧実装の「全ユーザー走査」=毎リク1000行scanを排除）。
	seen := make(map[int]struct{}, len(results))
	authorIDs := make([]int, 0, len(results))
	for _, p := range results {
		if _, ok := seen[p.UserID]; !ok {
			seen[p.UserID] = struct{}{}
			authorIDs = append(authorIDs, p.UserID)
		}
	}
	userMap := make(map[int]User, len(authorIDs))
	if err := selectUsersInto(ctx, userMap, authorIDs); err != nil {
		return nil, err
	}

	// 投稿者が削除されていない投稿を最大 postsPerPage 件まで選択（旧実装の挙動を維持）
	posts := make([]Post, 0, postsPerPage)
	for _, p := range results {
		author := userMap[p.UserID]
		if author.DelFlg != 0 {
			continue
		}
		p.User = author
		p.CSRFToken = csrfToken
		// Step14: テンプレ側の関数呼び出し/メソッド呼び出し(reflect.Value.Call)を消すため事前計算
		p.ImageURL = imageURL(p)
		p.CreatedAtFmt = p.CreatedAt.Format(ISO8601Format)
		posts = append(posts, p)
		if len(posts) >= postsPerPage {
			break
		}
	}
	if len(posts) == 0 {
		return posts, nil
	}

	postIDs := make([]int, len(posts))
	for i := range posts {
		postIDs[i] = posts[i].ID
	}

	// コメント本体を一括取得（post_idごとに created_at 降順）。!allComments なら各投稿の最新3件のみ採用。
	// Step18: コメント件数(CommentCount)はこの全件取得から Go 側で数える。本体クエリが既に各投稿の
	// 全コメントを返すため、別途の `COUNT(*) GROUP BY` クエリ(digest 7.7s/16k回)は冗長＝排除（結果同一）。
	countMap := make(map[int]int, len(posts))
	commentMap := make(map[int][]Comment, len(posts))
	{
		q, args, err := sqlx.In("SELECT * FROM `comments` WHERE `post_id` IN (?) ORDER BY `post_id`, `created_at` DESC, `id` DESC", postIDs)
		if err != nil {
			return nil, err
		}
		var comments []Comment
		if err := db.SelectContext(ctx, &comments, db.Rebind(q), args...); err != nil {
			return nil, err
		}
		for _, c := range comments {
			countMap[c.PostID]++ // 全コメントを数える（表示用に3件残すかは下で判定）
			if !allComments && len(commentMap[c.PostID]) >= 3 {
				continue
			}
			commentMap[c.PostID] = append(commentMap[c.PostID], c)
		}
	}

	// コメント投稿者のユーザー情報を追加取得（投稿者と重複しない未取得idのみ）
	commentAuthorIDs := make([]int, 0)
	for _, cs := range commentMap {
		for _, c := range cs {
			if _, ok := seen[c.UserID]; !ok {
				seen[c.UserID] = struct{}{}
				commentAuthorIDs = append(commentAuthorIDs, c.UserID)
			}
		}
	}
	if err := selectUsersInto(ctx, userMap, commentAuthorIDs); err != nil {
		return nil, err
	}

	// 各投稿へ割り当て。コメントは投稿者情報を埋め、降順取得後に反転して旧実装と同じ表示順にする
	for i := range posts {
		posts[i].CommentCount = countMap[posts[i].ID]
		comments := commentMap[posts[i].ID]
		for j := range comments {
			comments[j].User = userMap[comments[j].UserID]
		}
		for a, b := 0, len(comments)-1; a < b; a, b = a+1, b-1 {
			comments[a], comments[b] = comments[b], comments[a]
		}
		posts[i].Comments = comments
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func mimeToExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	}
	return ""
}

// 画像を public/image/{id}.{ext} に書き出して以後 nginx が静的配信できるようにする。
// アプリの WorkingDirectory は webapp/golang なので ../public を指す。
func saveImageFile(id int64, mime string, data []byte) {
	ext := mimeToExt(mime)
	if ext == "" {
		return
	}
	_ = os.WriteFile(fmt.Sprintf("../public/image/%d.%s", id, ext), data, 0644)
}

// 初期データ外(id>10000)の画像ファイルを削除し、/initialize 後の状態をクリーンに保つ。
// dbInitialize が posts WHERE id>10000 を削除するのに合わせる（ディスク肥大・再起動耐性対策）。
func cleanupImageFiles() {
	entries, err := os.ReadDir("../public/image")
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		dot := strings.IndexByte(name, '.')
		if dot <= 0 {
			continue
		}
		id, err := strconv.Atoi(name[:dot])
		if err != nil {
			continue
		}
		if id > 10000 {
			os.Remove("../public/image/" + name)
		}
	}
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

// テンプレートは起動時に1回だけパースする。旧実装はハンドラ毎に template.Must(ParseFiles) で
// 毎リクエスト file I/O + パースしており app(Go) CPU の主因だった。
// html/template はパース完了後の Execute が並行安全。
var tmplFuncs = template.FuncMap{
	"imageURL": imageURL,
}

var (
	tmplLogin    = template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFiles(getTemplPath("layout.html"), getTemplPath("login.html")))
	tmplRegister = template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFiles(getTemplPath("layout.html"), getTemplPath("register.html")))
	tmplIndex    = template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFiles(getTemplPath("layout.html"), getTemplPath("index.html"), getTemplPath("posts.html"), getTemplPath("post.html")))
	tmplUser     = template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFiles(getTemplPath("layout.html"), getTemplPath("user.html"), getTemplPath("posts.html"), getTemplPath("post.html")))
	tmplPosts    = template.Must(template.New("posts.html").Funcs(tmplFuncs).ParseFiles(getTemplPath("posts.html"), getTemplPath("post.html")))
	tmplPostID   = template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFiles(getTemplPath("layout.html"), getTemplPath("post_id.html"), getTemplPath("post.html")))
	tmplBanned   = template.Must(template.New("layout.html").Funcs(tmplFuncs).ParseFiles(getTemplPath("layout.html"), getTemplPath("banned.html")))
)

func getInitialize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbInitialize(ctx)
	cleanupImageFiles()
	// Step17: dbInitialize が del_flg を再設定するため userCache を全クリア（stale防止）。
	userCacheMu.Lock()
	userCache = make(map[int]User)
	userCacheMu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	tmplLogin.Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(ctx, r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	tmplRegister.Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.GetContext(ctx, &exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.ExecContext(ctx, query, accountName, calculatePasshash(ctx, accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)

	results := []Post{}

	err := db.SelectContext(ctx, &results, "SELECT p.id, p.user_id, p.body, p.mime, p.created_at FROM `posts` p FORCE INDEX (idx_created_at) STRAIGHT_JOIN `users` u ON p.user_id = u.id WHERE u.del_flg = 0 ORDER BY p.created_at DESC LIMIT 20")
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	tmplIndex.Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountName := r.PathValue("accountName")
	user := User{}

	err := db.GetContext(ctx, &user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT 20", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := 0
	err = db.GetContext(ctx, &commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := []int{}
	err = db.SelectContext(ctx, &postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []any
		args := make([]any, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		err = db.GetContext(ctx, &commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	tmplUser.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.SelectContext(ctx, &results, "SELECT p.id, p.user_id, p.body, p.mime, p.created_at FROM `posts` p FORCE INDEX (idx_created_at) STRAIGHT_JOIN `users` u ON p.user_id = u.id WHERE p.created_at <= ? AND u.del_flg = 0 ORDER BY p.created_at DESC LIMIT 20", t.Format(ISO8601Format))
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	tmplPosts.Execute(w, posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.SelectContext(ctx, &results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(ctx, results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	tmplPostID.Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Step16: 画像はFSにのみ保存し、DBのimgdata BLOBには書かない（DB最大コスト=INSERT posts
	// 16s/avg31msの排除）。配信はnginx静的(try_files)＝Step3で既にFS化済。imgdataはNOT NULLの
	// ため空バイト列を入れる（行サイズが激減しINSERTがµs級に）。getImageのDBフォールバックは
	// 新規投稿では走らない（下のsaveImageFileがredirect前に同期書込みするためFSに必ず存在）。
	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.ExecContext(
		ctx,
		query,
		me.ID,
		mime,
		[]byte{},
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	// 投稿画像をファイルに書き出し、以後 nginx が静的配信する（DB BLOBの代替＝唯一の保存先）
	saveImageFile(pid, mime, filedata)

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pidStr := r.PathValue("id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	post := Post{}
	err = db.GetContext(ctx, &post, "SELECT `mime`, `imgdata` FROM `posts` WHERE `id` = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	ext := r.PathValue("ext")

	if ext == "jpg" && post.Mime == "image/jpeg" ||
		ext == "png" && post.Mime == "image/png" ||
		ext == "gif" && post.Mime == "image/gif" {
		// write-through: 次回以降は nginx が静的配信できるようファイルへ書き出す
		saveImageFile(int64(pid), post.Mime, post.Imgdata)
		w.Header().Set("Content-Type", post.Mime)
		_, err := w.Write(post.Imgdata)
		if err != nil {
			log.Print(err)
			return
		}
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.ExecContext(ctx, query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.SelectContext(ctx, &users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	tmplBanned.Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.ExecContext(ctx, query, 1, id)
		// Step17: ban した id を userCache から削除（次アクセスで del_flg=1 を再読込）。
		if n, err := strconv.Atoi(id); err == nil {
			userCacheMu.Lock()
			delete(userCache, n)
			userCacheMu.Unlock()
		}
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	// Step16: 画像はFSが唯一の保存先（DB BLOB廃止）になったため、保存先dirの存在を起動時に保証。
	// dir不在だと os.WriteFile が無言失敗し画像が全滅する（Verifier指摘の安全網）。
	if err := os.MkdirAll("../public/image", 0755); err != nil {
		log.Printf("failed to ensure image dir: %s", err)
	}

	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	cfg.DBName = dbname
	cfg.Params = map[string]string{
		"charset": "utf8mb4",
	}
	cfg.ParseTime = true
	cfg.Loc = time.Local
	cfg.InterpolateParams = true // prepare+exec の2往復を排除（クライアント側でパラメータ展開）
	dsn := cfg.FormatDSN()

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	// 接続プール: 既定の MaxIdleConns=2 による接続の張り直し(再ハンドシェイク)を排除。
	db.SetMaxOpenConns(100)
	db.SetMaxIdleConns(100)
	db.SetConnMaxLifetime(0)
	defer db.Close()

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[0-9a-zA-Z_]+}`, getAccountName)
	r.Mount("/", http.FileServer(http.Dir("../public")))

	// Step10: pprof（診断用）。DefaultServeMux に登録される net/http/pprof を localhost:6060 で公開。
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	log.Fatal(http.ListenAndServe(":8080", r))
}
