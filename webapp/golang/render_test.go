package main

import (
	"bytes"
	"html/template"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Step19のレギュレーション担保: 手書きレンダラ(renderPosts/renderPostOne)の出力が
// 実際の html/template(posts.html + post.html)の出力と「バイト完全一致」することを検証する。
// 敵対的入力(HTML特殊文字 <>&"' を含む body/comment、0件コメント、空body、多バイト)で突き合わせ、
// 一致しなければ失敗＝採用しない。これが「html/template と等価」の機械的証明になる。

func jst() *time.Location { return time.FixedZone("JST", 9*3600) }

func mkPost(id int, body string, comments []Comment) Post {
	t := time.Date(2016, 1, 2, 11, 46, 21, 0, jst())
	return Post{
		ID:           id,
		UserID:       1,
		Body:         body,
		Mime:         "image/jpeg",
		CreatedAt:    t,
		CommentCount: len(comments),
		Comments:     comments,
		User:         User{ID: 1, AccountName: "alice_01"},
		CSRFToken:    "abc123def4567890",
		ImageURL:     "/image/" + strconv.Itoa(id) + ".jpg",
		CreatedAtFmt: t.Format(ISO8601Format),
	}
}

func samplePosts() []Post {
	c1 := Comment{Comment: `hi <b>"&'</b> ねこ`, User: User{AccountName: "bob_02"}}
	c2 := Comment{Comment: "plain comment", User: User{AccountName: "carol_3"}}
	c3 := Comment{Comment: `<script>alert(1)</script>`, User: User{AccountName: "dave_99"}}
	return []Post{
		mkPost(1, `body with <script>&"' タグ + 1`, []Comment{c1, c2, c3}),
		mkPost(2, "no comments here", nil),
		mkPost(3, "", []Comment{c2}),
		mkPost(4, `a & b < c > d " e ' f + g`, []Comment{}),
	}
}

func buildRealPostsTmpl(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("posts.html").
		Funcs(template.FuncMap{"imageURL": imageURL}).
		ParseFiles("templates/posts.html", "templates/post.html"))
}

func buildRealPostTmpl(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("post.html").
		Funcs(template.FuncMap{"imageURL": imageURL}).
		ParseFiles("templates/post.html"))
}

func firstDiff(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

func around(s string, i int) string {
	a := i - 40
	if a < 0 {
		a = 0
	}
	b := i + 40
	if b > len(s) {
		b = len(s)
	}
	return s[a:b]
}

func TestRenderPostsByteIdentical(t *testing.T) {
	tmpl := buildRealPostsTmpl(t)
	posts := samplePosts()

	var want bytes.Buffer
	if err := tmpl.Execute(&want, posts); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	got := string(renderPosts(posts))
	if got != want.String() {
		i := firstDiff(got, want.String())
		t.Fatalf("renderPosts != html/template at byte %d\nWANT…%q…\nGOT …%q…\nwant_len=%d got_len=%d",
			i, around(want.String(), i), around(got, i), want.Len(), len(got))
	}
}

func TestRenderPostsEmpty(t *testing.T) {
	tmpl := buildRealPostsTmpl(t)
	posts := []Post{}
	var want bytes.Buffer
	if err := tmpl.Execute(&want, posts); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	got := string(renderPosts(posts))
	if got != want.String() {
		i := firstDiff(got, want.String())
		t.Fatalf("empty renderPosts mismatch at %d\nWANT %q\nGOT  %q", i, around(want.String(), i), around(got, i))
	}
}

// TestIndexPageByteIdentical はページ全体(layout+index)で、旧方式
// ({{ template "posts.html" .Posts }}) と新方式({{ .PostsHTML }}+renderPosts) の
// 出力がバイト一致することを検証する＝統合後もレギュレーション担保。
func TestIndexPageByteIdentical(t *testing.T) {
	fm := template.FuncMap{"imageURL": imageURL}
	// 新方式: 現行 index.html（{{ .PostsHTML }}）
	newTmpl := template.Must(template.New("layout.html").Funcs(fm).
		ParseFiles("templates/layout.html", "templates/index.html"))

	// 旧方式: index.html の {{ .PostsHTML }} を {{ template "posts.html" .Posts }} に戻した一時テンプレ
	idx, err := os.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	oldIdx := strings.Replace(string(idx), "{{ .PostsHTML }}", `{{ template "posts.html" .Posts }}`, 1)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/index.html", []byte(oldIdx), 0644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"layout.html", "posts.html", "post.html"} {
		b, err := os.ReadFile("templates/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/"+f, b, 0644); err != nil {
			t.Fatal(err)
		}
	}
	oldTmpl := template.Must(template.New("layout.html").Funcs(fm).
		ParseFiles(dir+"/layout.html", dir+"/index.html", dir+"/posts.html", dir+"/post.html"))

	posts := samplePosts()
	me := User{ID: 5, AccountName: "me_user", Authority: 1}
	csrf := "tok_csrf_xyz"
	flash := `notice <&">`

	var oldBuf, newBuf bytes.Buffer
	if err := oldTmpl.Execute(&oldBuf, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, csrf, flash}); err != nil {
		t.Fatalf("old execute: %v", err)
	}
	if err := newTmpl.Execute(&newBuf, struct {
		PostsHTML template.HTML
		Me        User
		CSRFToken string
		Flash     string
	}{renderPosts(posts), me, csrf, flash}); err != nil {
		t.Fatalf("new execute: %v", err)
	}
	if oldBuf.String() != newBuf.String() {
		i := firstDiff(oldBuf.String(), newBuf.String())
		t.Fatalf("index full-page mismatch at byte %d\nOLD…%q…\nNEW…%q…", i, around(oldBuf.String(), i), around(newBuf.String(), i))
	}
}

func TestRenderPostOneByteIdentical(t *testing.T) {
	tmpl := buildRealPostTmpl(t)
	p := samplePosts()[0]
	var want bytes.Buffer
	if err := tmpl.ExecuteTemplate(&want, "post.html", p); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	got := string(renderPostOne(p))
	if got != want.String() {
		i := firstDiff(got, want.String())
		t.Fatalf("renderPostOne mismatch at %d\nWANT…%q…\nGOT …%q…", i, around(want.String(), i), around(got, i))
	}
}
