# private-isu チューニング戦績ログ

## 環境
- インスタンス: AWS c6i.large (2 vCPU / 3.7GB RAM / swapなし), Ubuntu 24.04
- 実装: Go 1.26.3 (chi + sqlx + gomemcache), nginxの背後 app:8080
- ミドルウェア: nginx:80 → app:8080 / MySQL 8.0.45 (localhost) / memcached (localhost)
- 接続: `ssh isucon@52.193.202.123`（SSHはNetskope経由・出口IP 158.95.54.208、SGで許可）
- ベンチ: `cd /home/isucon/private_isu/benchmarker && ./bin/benchmarker -u ./userdata -t http://localhost`（60秒）
- スコア式: 成功GET×1 + 成功POST×2 + 画像投稿×5 − (5xx×10 + exception×20)
- DB初期サイズ: posts 9,397行/1,204.5MB(画像BLOB), comments 99,918行, users 1,000行

---

## スコア推移

| # | 施策 | score | success | fail | 備考 |
|---|---|---|---|---|---|
| 参考 | Ruby初期(slowログOFF) | 634 | 571 | 2 | 主催配布の初期計測値 |
| 0 | Go初期ベースライン | 0 | 643 | 55 | 索引なしN+1でMySQL飽和→timeout多発 |
| 1 | インデックス追加 | **16028** | 15078 | 0 | comments(post_id,created_at)他。fail0達成 |
| 2 | openssl除去(crypto/sha512) | 17891 | 16789 | 0 | パスワードハッシュをGo内製化。fail0維持 |
| 3 | 画像をFS化+nginx静的配信(expires) | 29764 | 28445 | 0 | warm中央値。冷間初回は~21k。expiresで+30% |
| 4 | makePosts N+1解消 + initialize画像cleanup | 31085 | 29847 | 0 | warm中央値。disk逼迫を解消後の安定値(+4.4%) |
| 5 | 投稿系クエリJOIN+LIMIT20 / getPostsID imgdata除外 / comments(user_id)索引 | **43763** | 42258 | 0 | warm中央値(+40.8%)。全1万行fetch→20行が主効果 |

---

## Step 0: ベースライン (Go) — score 0

### 計測結果
```
{"pass":true,"score":0,"success":643,"fail":55}
```
timeout: GET /, GET /@user, POST /login, POST /register

### 考察（slow query log を long_query_time=0 で全クエリ採取して解析）

| クエリ | 回数 | 合計時間 | 1回平均 |
|---|---|---|---|
| `SELECT * FROM comments WHERE post_id=? ORDER BY created_at DESC LIMIT N` | 873 | 434秒 | 0.50s |
| `SELECT COUNT(*) FROM comments WHERE post_id=?` | 946 | 140秒 | 0.15s |
| `SELECT * FROM comments WHERE post_id=? ORDER BY created_at DESC` | 57 | 22秒 | 0.39s |
| `SELECT id,user_id,body,mime,created_at FROM posts ORDER BY created_at DESC` | 53 | 9秒 | 1万行/回返却 |
| `SELECT * FROM users WHERE id=?` | 3877 | - | N+1 |

### 原因
- **comments(post_id) にインデックスが無い** → 10万行のフルスキャンが多発（合計約600秒のDB時間）。
- makePosts のN+1（投稿ごとにcount/comments/各コメントのuser/投稿のuserを個別SELECT）。
- Goの database/sql が接続を無制限に張り、索引なしフルスキャンが殺到してMySQLが飽和→タイムアウト（Rubyはworker数固定で偶然634が出ていた）。

---

## Step 1: インデックス追加 — score 16028 (fail 0)

### 施策（DDL。サーバ `/home/isucon/tuning/01_indexes.sql` にも保存）

```sql
ALTER TABLE comments ADD INDEX idx_post_created (post_id, created_at);
ALTER TABLE posts ADD INDEX idx_user_id (user_id);
ALTER TABLE posts ADD INDEX idx_created_at (created_at);
```

### Before → After

| 指標 | Before (Step 0) | After (Step 1) |
|---|---|---|
| score | 0 | **16028** |
| success | 643 | 15078 |
| fail | 55 | **0** |

### 検証
- `EXPLAIN SELECT * FROM comments WHERE post_id=? ORDER BY created_at DESC LIMIT 3`：`key=idx_post_created, type=ref, rows=12`（旧: 10万行フルスキャン）。

### 考察
- 索引なしフルスキャンが圧倒的主犯だった。索引追加でタイムアウトが完全消滅し、約25倍のスコアを達成。
- インデックスはMySQLデータディレクトリに永続化されるため再起動耐性あり。

### 次の最適化候補（未適用）
- ② Goのコネクションプール制限 (`SetMaxOpenConns`)
- ③ 画像をDB(1.2GB BLOB)→ファイルシステム化＋nginx直配信
- ④ makePostsのN+1解消（comments/usersを一括取得 IN/JOIN、users 3877回SELECT撲滅）
- ⑤ パスワードハッシュ openssl外部プロセス → Go `crypto/sha512`
- ⑥ nginx(静的配信/gzip)
- ⑦ MySQL設定チューニング
- ⑧ memcachedキャッシュ活用

---

## Step 2: openssl除去（パスワードハッシュをGo内製化） — score 17891

### 背景（再プロファイルで判明した索引適用後のボトルネック）
索引適用後(score16100)のnginx LTSV集計（60秒・合計処理時間）:

| エンドポイント | 合計(s) | 回数 | 平均 |
|---|---|---|---|
| GET / | 224 | 547 | 409ms |
| GET /image/*.jpg | 104 | 6553 | 16ms |
| GET /posts | 66 | 115 | 571ms |
| GET /image/*.png | 59 | 2281 | 26ms |
| GET /posts/* | 55 | 762 | 72ms |
| POST /login | 24 | 404 | 60ms |

POST /login が openssl を外部プロセスで2回起動（salt用＋passhash用）していた。

### 施策（webapp/golang/app.go）
- `digest()` を openssl外部プロセス起動から **Goの crypto/sha512** に置換（`sha512.Sum512` + `encoding/hex` で小文字hex。`openssl dgst -sha512` の出力と同一なので既存passhashと互換）。
- 不要になった `escapeshellarg()` と `os/exec` importを削除、`crypto/sha512`/`encoding/hex` を追加。

```go
func digest(ctx context.Context, src string) string {
	sum := sha512.Sum512([]byte(src))
	return hex.EncodeToString(sum[:])
}
```

- 適用方法: ローカルrepoで編集→SSH標準入力(`ssh … 'cat > app.go' < local`)でEC2へ送信→サーバで`go build`→差し替え→`systemctl restart isu-go`。

### Before → After

| 指標 | Before (Step 1) | After (Step 2) |
|---|---|---|
| score | 16100 | **17891** |
| success | 15157 | 16789 |
| fail | 0 | **0** |

### 検証
- `go build` 成功（コンパイル検証）。ベンチ fail 0 で既存ユーザのログインが正常（passhash互換を確認）。

### 考察
- ログイン毎の fork+exec×2 が消え、CPU/プロセス生成コストが削減。POST /login の60msが短縮され全体スループット向上。

### 次の最適化候補（未適用）
- ③ 画像をDB BLOB→FS化＋nginx静的配信（try_files fallback, immutable, 304自動）… 最多リクエスト/約163s
- ④ makePosts N+1解消（GET / 224s 等の最大バケット）
- ② Go接続プール制限 / ⑥ nginx静的最適化 / ⑦ MySQL設定 / ⑧ memcached

---

## Step 3: 画像をDB BLOB→FS化 + nginx静的配信 — score 29764 (warm中央値)

### 背景（Step2後の再プロファイル）
GET /image が jpg 104s/6553req + png 59s/2281req = 約163s・8834リクエストで最多。getImage が `SELECT * FROM posts`(mediumblob) をGoで配信していた。

### 施策

1. **既存画像の一括FS書き出し**: `webapp/golang/cmd/dumpimages/main.go` でDBの全imgdataを `public/image/{id}.{ext}` へ出力（10062件/1.3GB/8.3秒）。
2. **app.go**: `getImage` を `SELECT mime,imgdata` に絞り、配信時にファイルへ write-through。`postIndex` で投稿時にファイル書き出し。`mimeToExt`/`saveImageFile` ヘルパ追加。
3. **nginx**: `/image/` を `try_files $uri @app;` で静的配信（無ければGoへフォールバック）＋ `expires 1d;`。

```nginx
location /image/ { try_files $uri @app; expires 1d; }
location @app { proxy_set_header Host $host; proxy_pass http://localhost:8080; }
```

### Before → After（warm中央値）

| 指標 | Before (Step 2) | After (Step 3) |
|---|---|---|
| score | 17891 | **29764** |
| success | 16789 | 28445 |
| fail | 0 | **0** |

`/image/1.jpg` は nginx静的配信を確認（Last-Modified/ETag/Accept-Ranges）。

### 実験：Cache-Control(expires) の効果を controlled 比較（温間×3）

| 構成 | scores | 中央値 |
|---|---|---|
| expires ON | 29914/29656/30073, 29577/29918/29764 | ~29.8k |
| expires OFF | 23485/23059/22864 | 23059 |

→ **expiresで約 23k→30k（+30%）**。ベンチがクライアントキャッシュを尊重し画像再取得をスキップするため。correctnessは安全（画像はid単位で不変、新規投稿は別URLでキャッシュミス→即取得、fail0）。

### 学び：計測プロトコル
- スコアのrun間variance大（冷間初回~21k ↔ 温間~30k）。**ウォームアップ1回（破棄）＋3回計測の中央値**を以後の標準プロトコルとする。単発計測は誤判断を招く（本Stepで「expires無influence」と一度誤結論しかけた）。

### 検証
- nginx静的配信をヘッダで確認。`go build` 成功。ベンチ fail 0。再起動耐性: 画像ファイル・索引はディスク永続。

### 考察
- DBからのBLOB読み出し+Go配信が消え、nginxのsendfileで直接配信。さらにexpiresによるブラウザキャッシュ活用で画像再取得が激減し、約+67%（17891→29764）を達成。

### 次の最適化候補（未適用）
- ④ makePosts N+1解消（GET / が依然最大バケット）
- ② Go接続プール制限 (`SetMaxOpenConns`)
- ⑦ MySQL設定チューニング
- ⑧ memcachedキャッシュ活用

---

## Step 4: makePosts N+1解消 + /initialize 画像クリーンアップ — score 31085 (warm中央値)

### 背景
Step3後の再プロファイルで GET / が 291s と依然最大。makePosts が1ページ(最大20投稿)あたり約120クエリ（投稿ごとに COUNT・コメント・各コメント投稿者・投稿者を個別SELECT）を発行していた。

### 施策（webapp/golang/app.go）
1. **makePosts を一括取得に全面書き換え**:
   - `SELECT id, account_name, del_flg FROM users` で全ユーザーをmap化（コメント投稿者/投稿者の個別SELECTを排除）
   - 投稿者 del_flg=0 のみ最大20件選択（旧挙動維持）
   - `sqlx.In` で `comments WHERE post_id IN(...) GROUP BY post_id`（件数）と `... IN(...) ORDER BY post_id, created_at DESC, id DESC`（本体）を一括取得し、Go側でpost_id別に振り分け・最新3件・reverse
   - 1ページ約120クエリ → 約3クエリ
2. **getInitialize に画像クリーンアップ追加** (`cleanupImageFiles`): `posts WHERE id>10000` のDB削除に合わせ `public/image/{id>10000}` を削除。

### Before → After（warm中央値）

| 指標 | Before (Step 3) | After (Step 4) |
|---|---|---|
| score | 29764 | **31085** |
| success | 28445 | 29847 |
| fail | 0 | **0** |

GET / 平均は 376ms → 299ms、GET /posts は 554ms → 270ms に短縮。

### ⚠️ 計測の落とし穴（重要な学び）
当初の計測が 27906→21814→18179 と**run毎に逓減**し混乱した。真因は **ディスク96%満杯**：ベンチがPOSTする画像ファイルが毎回 `public/image/` に蓄積し（旧 /initialize はDB行のみ削除しファイルを残していた）、ディスク逼迫でFS/DBが劣化していた。
- 対処: 蓄積ゴミ(id>10000, 1830件/0.8GB)を削除 → disk 96%→90%。さらに getInitialize に cleanupImageFiles を追加し**毎initializeで自動クリーンアップ**（計測後もファイル数~10000で頭打ち、無限肥大しない）。
- 教訓: スコア逓減トレンドはvarianceではなくシステム劣化のサイン。FS化施策には**初期化時のファイル整合（再起動耐性）**が必須。

### 検証
- Verifier レビュー PASS（DOM不変・SQL等価・コメント順/reverse・del_flg・LIMIT3相当・POST即時反映・go build/vet OK）。
- 安定計測: 31126/31085/30337（fail 0）。disk 1.3G空き維持、画像ファイル数 ~10000 で安定。

### 考察
- N+1自体は解消したが、GET / は呼び出し側 `getIndex` の `SELECT ... FROM posts ORDER BY created_at DESC`（**LIMITなし=全1万行fetch**）が新たな主因として顕在化。伸びが+4.4%に留まったのはこのため。

### 次の最適化候補（未適用）
- **⑤(次) 投稿一覧クエリに LIMIT + del_flg を SQL側で**（getIndex/getPosts/getAccountName）: 1万行fetch→20行。GET / の本命。
- ② Go接続プール制限 / ⑦ MySQL設定 / ⑧ memcached

---

## Step 5: 投稿系クエリ JOIN+LIMIT20 / getPostsID imgdata除外 / comments(user_id)索引 — score 43763 (warm中央値)

### 背景
Step4でmakePostsのN+1自体は解消したが、呼び出し側のクエリが依然非効率だった:
- `getIndex`/`getPosts` が `SELECT ... FROM posts ORDER BY created_at DESC`（**LIMITなし＝全約1万行fetch**）して、del_flg判定と20件絞り込みをGo側(makePosts)で実施していた。GET / の本命ボトルネック。
- `getAccountName` のプロフィールページで `SELECT COUNT(*) FROM comments WHERE user_id=?` が **comments(user_id)索引なし→10万行フルスキャン**。加えて投稿一覧も LIMITなし。
- `getPostsID` が `SELECT *`（mediumblob imgdata込み）で、配信に使わない画像BLOBを毎回読み出し・転送していた（画像はStep3でFS/`/image/`配信化済み）。

### 施策（webapp/golang/app.go ＋ tuning/02_indexes.sql）
1. **getIndex / getPosts**: `JOIN users u ON p.user_id=u.id WHERE u.del_flg=0 ... ORDER BY p.created_at DESC LIMIT 20` に変更。del_flg判定と件数制限をDB側へ押し下げ、**全約1万行fetch→20行**に圧縮。
2. **getAccountName**: 投稿一覧クエリに `LIMIT 20` 追加（本人プロフィールページのため del_flg JOINは不要＝本人は非削除確定。統計の postCount/commentCount/commentedCount は LIMITなしの別クエリで算出するため不変）。
3. **getPostsID**: `SELECT *` → `SELECT id,user_id,body,mime,created_at`（**imgdata BLOB除外**）。getPostsID経由では imgdata 未参照のため無駄なBLOB読み出し・転送を排除。
4. **comments(user_id) 索引追加**（`tuning/02_indexes.sql`）: getAccountName の `COUNT(*) FROM comments WHERE user_id=?` の10万行フルスキャンを解消。Step1の idx_post_created(post_id,created_at) とは別列で非重複。

### Before → After（warm中央値）

| 指標 | Before (Step 4) | After (Step 5) |
|---|---|---|
| score | 31085 | **43763** |
| success | 29847 | 42258 |
| fail | 0 | **0** |

### 計測（温間プロトコル）
warm(discard) 44102 を破棄し、本計測3回: **43706 / 43763 / 44498**（各 fail 0、success ~42195/42258/42973）→ **中央値 43763**。直近サニティ43768とほぼ一致。disk 1.1G空き(93%使用)・fail全回0で実害なし（/initialize の画像クリーンアップが頭打ちを維持）。

### 検証（Verifier レビュー PASS）
- **makePosts等価性**: 新SQLが返す20件は全て del_flg=0 のため makePosts の author フィルタで1件も落ちず、旧実装（全件fetch→Goでskip+先頭20件）と同一集合・同一順序。3コメント制限・count・降順取得後のreverse経路も不変。
- **getAccountName**: 対象ユーザは del_flg=0 確認済みで全投稿が本人＝非削除。LIMIT20のみで等価。統計countは別クエリで不変。
- **getPostsID imgdata除外**: テンプレート(templates/*.html)に Imgdata 参照なし。Post.Imgdata を読むのは getImage（独自に `SELECT mime,imgdata` 発行）のみで getPostsID と独立。描画影響なし。
- **comments(user_id)索引**: base schemaは comments に PRIMARY(id)のみ。非重複・非衝突で COUNT クエリに直効。
- `go build ./...` / `go vet ./...` ともに exit 0。

### 考察
- LIMITなし全約1万行fetchを20行に圧縮したのが主効果（GET / の本命）。索引追加とBLOB除外の相乗で **+40.8%（31085→43763）**。N+1解消(Step4)で1ページのクエリ本数は減っていたが、1クエリあたりの転送行数が支配的に残っていたことが裏付けられた。

### 留保／次の最適化候補（未適用）
- ⚠️ `created_at` 同値タイ時の選択行は `ORDER BY created_at DESC`（2次ソート無し）+LIMITでMySQL任せの非決定性。ただし**旧実装も2次ソート無しで同じ**＝新規リグレッションではない（seedは created_at がほぼ一意で実害なし）。決定化したい場合は `created_at DESC, id DESC` の2次キー化。
- ② Go接続プール制限 (`SetMaxOpenConns`) / makePostsの「全ユーザー取得(`SELECT ... FROM users`)」を出現post_idのuser_idで`IN`絞り込み / ⑦ MySQL設定 / ⑧ memcached / 静的(css/js)のnginx配信。
- ⚠️ disk 使用率が93%まで上昇。ベンチ多数回後はディスク残量に注意（/initializeで頭打ちだが余裕は小さい）。
