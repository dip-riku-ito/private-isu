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
| 6 | タイムラインクエリ FORCE INDEX+STRAIGHT_JOIN（filesort排除） | **~132000** | ~127000 | 0 | warm clean2回値(約3倍)。GET/ 354→43.6ms(8x)。run3はディスク満杯でfail(infra) |
| 7 | disk恒久解放(binlog off)＋MySQL(buffer1G/flush2)＋nginx静的/gzip/keepalive＋FD上限 | **156127** | ~151000 | 0 | warm中央値(+18%)。mysqld130→44%、壁がapp(Go)へ移行 |
| 8 | DB接続プール(MaxOpen/Idle100)＋interpolateParams=true | **170721** | ~165000 | 0 | warm中央値(+9%)。mysqld %wait 2.6→0。app(Go)依然74%で壁 |
| 9 | テンプレート起動時1回パース | **173720** | ~168000 | 0 | warm中央値(+1.7%)。app(Go)74→70.7%。パースは主因でなく効果限定 |
| 10-11 | pprof診断→makePostsの全ユーザー走査(1000行)を必要user_idのIN取得へ | **204283** | ~195000 | 0 | warm中央値(+17.6%)。app(Go)70.7→50.3%、scanAll52→17%。壁がtemplate Execute(48%)へ |
| 12 | (不採用)コメントROW_NUMBER 3件限定 | 200860 | - | 0 | -1.7%。app→mysqld荷移動で純減→revert |
| 13 | /posts?max_created_at= の nginxキャッシュ(proxy_cache) | **231540** | ~221000 | 0 | warm中央値(+13.3%)。GET/posts 41→0.00ms(HIT~100%)。app/mysqld両-3pt・CPU~25%遊休=GET/レイテンシ律速へ |
| 14 | テンプレの reflect.Value.Call 排除（imageURL/CreatedAt.Format を事前計算フィールド化） | 229814 | ~219000 | 0 | warm中央値(横ばい・-0.7%=ノイズ内)。GET/ 13.16→12.55ms(-5%)・app46.6→45.6%は実現するもスコア不変＝micro-optの限界。出力バイト等価(Verifier PASS)・無害なので採用維持。※後の実測で頭打ちの真因は「**2vCPU CPU総量飽和**(負荷中idle 0%)」と判明（→Step15診断）。app1pt空けても飽和共有プールに即吸収 |
| 15 | 匿名(未ログイン)GET/ の nginxキャッシュ(proxy_cache・Cookie有はbypass) | **244635** | ~232000 | 0 | warm中央値(+6.4%・+14.8k)。GET/ 12.55→5.03ms(-60%)・匿名41%が0ms HIT。残59%は認証workerでBYPASS→Goへ(ban/CSRF安全)。Reg試算+105kに届かぬのは①cache対象が匿名41%のみ②CPU飽和で解放分が再吸収。fail0 |

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

---

## Step 6: タイムラインクエリの実行計画矯正（FORCE INDEX + STRAIGHT_JOIN）— score ~132,000 (warm, clean runs)

### 背景（Step5後の実機プロファイルで判明 / Agent Teams: Reg=規定 + Bench=実測 + Verifier=コード）
- **負荷モデルはレイテンシ律速**（Reg）: ベンチは60秒固定・11並列ハードコード・ランプアップ無し → **score ∝ 1/平均レイテンシ**。現状≈15ms/req、400kには≈1.6ms/req（約10倍短縮）が必要。
- **飽和tier=MySQL CPU**（Bench実測）: mysqld 129.7%/2vCPU（純CPU, %wait<1）, app 38.9%（うち22%はDB応答待ち）, nginx 8.2%, memcached 0.2%。app/nginxは余力ありDBが壁。
- **真因**: タイムラインposts クエリが1リクで1〜2万行スキャン（GET/ 354ms, GET/posts 387ms）。Step5でLIMIT20を足したのに重いのは、`JOIN users WHERE del_flg=0 ORDER BY created_at DESC LIMIT 20` をオプティマイザが「idx_created_at早期終了」せず「~1万行をfilesort」する計画を選んでいたため（JOINクエリ実測 avg313ms/~1.1万行）。

### 施策（webapp/golang/app.go）
getIndex/getPosts のタイムラインクエリを `FROM posts p FORCE INDEX (idx_created_at) STRAIGHT_JOIN users u ON p.user_id=u.id WHERE ... ORDER BY p.created_at DESC LIMIT 20` に変更（WHERE/ORDER BY/LIMITは不変）。STRAIGHT_JOINで結合順序をposts→usersに固定、FORCE INDEXでidx_created_atを強制し、**索引逆順走査による早期終了（~21行）**に矯正。

### EXPLAIN（矯正後）
- getIndex: posts `key=idx_created_at` / **filesort無し** / Backward index scan / rows≈199、users eq_ref(PRIMARY)
- getPosts: `key=idx_created_at` / **filesort無し** / range+Backward index scan（LIMIT20で早期終了）

### Before → After
| 指標 | Before (Step 5) | After (Step 6) |
|---|---|---|
| score | 43763 | **~132000（約3倍）** |
| GET / avg | 354ms | **43.6ms (8.1x)** |
| GET /posts avg | 387ms | **46.9ms (8.3x)** |
| fail | 0 | 0（clean runs） |

### 計測（温間）
warm(破棄)130921 / run1 133175(succ128279,fail0) / run2 131767(succ126891,fail0) / **run3 139728(fail727・無効)**。run3のfailはコードでなく**ディスク満杯（infra）**。clean代表値 **≈132k**。

### 検証（Verifier PASS）
- STRAIGHT_JOIN/FORCE INDEX はオプティマイザヒントのみ。返る結果集合・順序（del_flg=0 の最新20件）は **Step5と完全同一**（Step5 correctness を保持）。imagePerPageChecker(≥20件)充足。
- idx_created_at は 01_indexes.sql で永続（/initialize は索引をdropしない）。`go build`/`go vet` OK。
- 留保: FORCE INDEX は対象索引が無いとフォールバックせず500化 → 索引適用が運用前提。

### 考察
mysqld CPUを食い潰していた filesort/全行スキャンを断ち、read avg が約1/8に。スコア約3倍。レイテンシ律速モデル（score∝1/latency）の裏付け。

### ⚠️ 顕在化した運用課題: ディスク満杯（最優先・次対応）
スコア3倍＝書込みスループット増で run3 中に `df / = 100%`（残45MB）。MySQLが /tmp 一時ファイルを書けず POST 500/timeout で fail727（GET系は正常）。要因: /var/lib/mysql 5.9G（**binlog 43本**＋posts.imgdata BLOB残存）, /home/isucon 5.2G（public/image 1.2G/1万枚）。
- 安全是正: `PURGE BINARY LOGS`＋nginxアクセスログ切詰（sudo/SSH書込み＝**ユーザー認可待ち**。自動実行は分類器が拒否）。
- 恒久是正（Step8/10）: binlog無効化（mysql再起動）、posts.imgdata列DROP（コード側でBLOB書込み停止後）。

### 次の最適化候補
1. **（運用・最優先）ディスク是正** → 計測再開可能に
2. Step7: makePosts の全ユーザー走査（67811回/1.6ms）をキャッシュ or IN絞り
3. Step8: MySQL設定（buffer_pool 128MB→1GB / binlog OFF / flush_log_at_trx_commit=2）
4. Step9: app/nginx（interpolateParams+接続プール / テンプレ起動時1回パース / 静的css·js·faviconをnginx直配信+expires(304=+1点)+gzip+upstream keepalive）
5. ⚠️ ベンチ同一2vCPUホスト相乗りの頭打ち（真の400kはベンチ別ホスト化が事実上前提）

---

## Step 7: disk恒久解放 + MySQL設定 + nginx静的/gzip/keepalive + FD上限 — score 156,127 (warm中央値)

### 背景
Step6でmysqld CPU飽和を解消した直後、ベンチ走行で**ディスク100%枯渇**（binlog肥大）し計測不能に。これを恒久解決しつつ、Verifierのコード監査で挙がっていたapp/nginx層の構造的ムダ（静的Go経由・gzip/keepalive無し）をまとめて是正（ユーザーが runbook を `!` 実行）。
- ⚠️ /initialize の画像cleanup(Step4)はFS投稿画像(id>10000)のみ対象。**binlog・nginxログ・posts.imgdata BLOB は対象外**で、これがディスク逼迫の真因（InnoDBはDELETE済み領域もOSへ返さない）。

### 施策（インフラ。tuning/STEP7_apply.sh, STEP7b_nginx_fd.sh, my.cnf, nginx-isucon.conf）
1. **disk恒久解放**: nginxログ切詰(116MB)＋`PURGE BINARY LOGS`＋`disable_log_bin`（binlog無効化で再増殖停止）→ **100%→70%**。
2. **MySQL**: innodb_buffer_pool_size 128MB→1GB / innodb_flush_log_at_trx_commit 1→2（書込みレイテンシ短縮、redo fsyncを毎秒1回に）。
3. **nginx**: 静的css/js/faviconをnginx直配信（root=public, expires 1d, access_log off）＝**Go負荷オフロード＋304加点**。gzip（text系）。**upstream keepalive 64＋HTTP/1.1**（Goへの接続再利用）。
4. **⚠️落とし穴→Step7bで修正**: 静的直配信＋高スループットで nginx が `Too many open files`(FD soft=1024) に達しaccept不可→全方位timeoutで **38kに崩落**。`worker_rlimit_nofile 65535` / `worker_connections 8192` / systemd `LimitNOFILE=65535`（daemon-reload+nginx restart）で是正。

### Before → After（warm中央値）
| 指標 | Before (Step 6) | After (Step 7+7b) |
|---|---|---|
| score | ~132000 | **156127（+18%）** |
| success | ~127000 | ~151000 |
| fail | 0 | 0 |

### 計測（FD修正後）
warm(破棄)160079 / run1 156127 / run2 159352 / run3 154390（全fail0）→ **中央値156127**。`Too many open files` 再発0。disk 71%安定（binlog off効果で増えず）。

### 適用確認
- MySQL: `@@innodb_buffer_pool_size=1GB / @@log_bin=OFF / @@innodb_flush_log_at_trx_commit=2` 確認。
- 静的nginx直配信: favicon.ico・css とも `Server: nginx` + `Expires` + `Cache-Control: max-age=86400`（Go非経由）。LTSV静的行 ~1万→92行に激減。
- 索引健在（EXPLAIN filesort無し rows=199）。

### 🎯 ボトルネックの移行（pidstat %CPU, 2vCPU=200%）
- **app(Go): 72.9%（usr61.9/sys11.1/%wait13.6）← 新ボトルネック（最大消費）**
- mysqld: 44.4%（Step5の129.7%→**約1/3**。索引＋buffer pool効果）
- nginx: 13.1%（%wait~30=upstream待ち） / memcached 0.76% / bench同居 13.2%
- GET /@user 76.4ms が単発最遅。
→ 律速は **mysqld → app(Go) CPU** へ移行。

### 考察・教訓
- インフラ一括（disk/MySQL/nginx）。+18%は静的Goオフロード＋keepalive＋MySQL書込み改善の複合（バンドルのため内訳は厳密分離せず）。
- **教訓: 高速化でスループットが上がると、隠れていた上限が次々顕在化する。Step6→7で「DB律速→disk枯渇→FD枯渇→app律速」と壁が4段移動した。** 各段で実測（CPU占有/エラーログ/df）が無ければ誤診していた。

### 計測プロトコル更新
- ユーザー要望により次回計測から **ウォームアップ1回＋本計測2回の中央値**（従来3回から短縮）。

### 次の最適化候補（app(Go) CPU削減が本命）
1. **Step8(次): DB接続プール（SetMaxOpenConns/Idle）＋ DSN `interpolateParams=true`** — MaxIdleConns既定2による接続churnと、全クエリprepare+exec2往復を解消。%wait13.6＋driver CPU削減。小変更・高確度[大]。
2. Step9: テンプレートの起動時1回パース（毎リク `template.Must(ParseFiles)` → CPU/file I/O削減。高頻度HTML経路）。
3. makePostsの全ユーザー走査(毎リクSELECT users)キャッシュ/IN絞り、/@userの重い集計(76ms)軽量化、pprofでホットパス特定。

---

## Step 8: DB接続プール + interpolateParams — score 170,721 (warm中央値)

### 背景
Step7で壁が app(Go) CPU(72.9%) へ移行。Goのリクエスト処理コスト削減フェーズの初手として、低リスク・高確度な接続層の最適化（Verifier監査#2）を実施。

### 施策（webapp/golang/app.go db初期化）
- `cfg.InterpolateParams = true`: 全クエリの **prepare+exec 2往復 → クライアント側補間で1往復**に。MySQLプロトコル待ち・driver CPU削減。
- `SetMaxOpenConns(100)/SetMaxIdleConns(100)/SetConnMaxLifetime(0)`: 既定 MaxIdleConns=2 による**接続張り直し(再ハンドシェイク)を排除**。

### Before → After（warm中央値・新プロトコル warmup+本2回）
| 指標 | Step 7 | Step 8 |
|---|---|---|
| score | 156127 | **170721 (+9%)** |
| success | ~151000 | ~165000 |
| fail | 0 | 0 |
| GET / avg | 45.5ms | 41.0ms |
| GET /posts avg | 49.9ms | 42.1ms |
| GET /@user avg | 76.4ms | 62.3ms |

### CPU（pidstat %CPU, 2vCPU=200%）
- app(Go) 74.1%（変化小・**依然ボトルネック**, usr64.5）
- mysqld 43.6%・**%wait 2.6→0.0**（prepare往復消失でMySQLプロトコル待ち解消）

### 検証（Verifier PASS）
interpolateParams はバイナリ`[]byte`(imgdata)含め安全（driver v1.10.0 が `_binary'...'` 補間・結果集合完全同一、utf8mb4は安全charset、複文/LOAD DATA無し）。プール値妥当(max_conn151>100)。`go build`/`go vet` OK。
留保: ConnMaxLifetime=0 は本番常駐なら wait_timeout 未満推奨（ベンチでは実害なし）。複数appインスタンス時は MaxOpen×台数≤max_conn に再調整。

### 考察
接続往復/churn削減で +9%・各avg 10〜18%短縮。だが tier は変わらず **app(Go) CPU(74%, usr64.5＝ユーザー空間支配)**。次の本丸は**テンプレレンダリング/makePosts処理そのもの**のCPU削減。

### 次（Step9）
**テンプレートの起動時1回パース**（現状ハンドラ毎に `template.Must(ParseFiles)`＝毎リクfile I/O+パース。高頻度HTML経路 GET//posts/@user のusr CPU直撃）。

---

## Step 9: テンプレートの起動時1回パース — score 173,720 (warm中央値)

### 背景
Step8後も app(Go) usr CPU(64.5%) が壁。Verifier監査#6（ハンドラ毎の `template.Must(ParseFiles)`＝毎リク file I/O+パース）を解消。

### 施策（webapp/golang/app.go）
7テンプレート（login/register/index/user/posts/post_id/banned）を package-level var で**起動時に1回パース**、ハンドラは `Execute` のみに。`fmap`(={"imageURL":imageURL})を `tmplFuncs` に共通化。

### Before → After（warm中央値, warmup+2）
| 指標 | Step 8 | Step 9 |
|---|---|---|
| score | 170721 | **173720 (+1.7%)** |
| fail | 0 | 0 |
| app(Go) %CPU | 74.1 | 70.7 (-3.4pt) |
| GET / avg | 41.0ms | 38.7ms |

### 検証（Verifier PASS）
7経路すべて旧と同じ named template・同じファイル集合を Execute＝**HTML出力完全同一**。funcmap付与（login/register/banned）は未参照で無害。html/template はパース後 Execute 並行安全。init時パースは cwd=webapp/golang で成立・テンプレ欠落時 fail-fast。go build/vet OK。

### 考察・方針転換
- テンプレパース排除は app CPU を **-3.4pt / +1.7%** に留まり、**パースは主因ではなかった**。
- app(Go)施策の逓減（interpolateParams +9% → テンプレ +1.7%）＝usr CPUは分散。**当て推量を止め、pprofでホットパスを実測特定してから本丸に当てる**方針に転換（Step10）。
- 残る app(Go) 候補: makePostsの全ユーザー走査(毎リク `SELECT * users`)＋処理ループ、template Execute(描画)、/@userの集計クエリ群。

### 次（Step10）
**pprof でCPUプロファイル採取** → Go内訳（Execute vs クエリ vs ループ）を実測し、最大消費関数を特定して Step11 で狙い撃ち。

---

## Step 10: pprof 診断（CPUプロファイル）

### 施策
app.go に `_ "net/http/pprof"` import ＋ main で `go http.ListenAndServe("localhost:6060", nil)`（localhost束縛・chiの:8080とは別、外部非公開）。ベンチ走行中に `go tool pprof -top -seconds=30 http://localhost:6060/debug/pprof/profile` で採取。

### 結果（cum%）— ホットパス実測
- **main.makePosts 57.7%**（全CPUの過半）。うち **sqlx.SelectContext→scanAll 52.4%**（DB行のreflectionスキャン）。
- html/template.Execute 22.0% / runtime.mallocgc 21.9%（上記由来GC）。
- ハンドラ別: getIndex 42% / getPosts 20% / getPostsID 16%。
- JSON/regexp/memcache/session は上位外（意外な消費なし）。
→ 本丸は **makePosts が毎リク scan する行数**（特に `SELECT * FROM users` 全1000行）。N+1（クエリ数）でなく**行スキャン量**が問題と確定。

---

## Step 11: makePosts の全ユーザー走査を必要 user_id の IN 取得へ — score 204,283 (warm中央値)

### 背景（ユーザー指摘も反映）
「N+1バッチ化＝根本解決か？」の議論を経て整理: **コメントN+1は Step4で解消済**。pprofの scanAll 52% の正体は「クエリ数」でなく **`SELECT * FROM users`（毎リク全1000行）を reflection scan** していたこと。これを必要idだけに絞る（1000→数十行, 95%削減・ゼロリスク）。完全キャッシュ化(→0)は並行制御/無効化コストが残5%に見合わず、ここはIN絞りが適正サイズと判断。

### 施策（webapp/golang/app.go）
- ヘルパ `selectUsersInto(ctx, dest, ids)`: `SELECT id,account_name,del_flg FROM users WHERE id IN (ids)`。
- makePosts: 全ユーザー取得を廃止 → ①resultsの投稿者idをIN取得 ②del_flg=0で最大20件選択 ③コメント件数/本体(既存IN) ④コメント投稿者idを追加IN取得 ⑤割当。`len(results)==0` 早期return。

### Before → After（warm中央値, warmup+2）
| 指標 | Step 9 | Step 11 |
|---|---|---|
| score | 173720 | **204283 (+17.6%)** |
| fail | 0 | 0 |
| app(Go) %CPU | 70.7 | **50.3 (-20pt)** |
| mysqld %CPU | 45.3 | 50.5（拮抗） |
| makePosts/scanAll cum | 57.7/52.4% | **17.8/~17%** |

### 検証（Verifier PASS）
旧（全ユーザーmap）と新（必要idのみ）で出力完全等価（Goのmapゼロ値がorphan idで旧と同挙動、del_flgフィルタ・CommentCount・コメント3件/全件・反転順・コメント投稿者解決すべて一致）。go build/vet OK。

### 🎯 ボトルネック移動（pidstat / 再pprof）
- app(Go) 50.3% / mysqld 50.5%（**~50/50拮抗**）。
- 再pprof: makePosts DB scan 52%→17% に陥落。**新最大ホットパス = `html/template.Execute` 22%→47.9%**（reflect.Value.Call ~24%）。
- 1リクの実CPU低下（30sサンプル 23.0s→15.3s）。

### 考察
「行スキャン量を減らす」中層施策が +17.6% と最大級。N+1（クエリ数）でなく**スキャン行数・再計算**が本質だったことの裏付け。次は template Execute（描画）と拮抗する mysqld。

### 次（Step12 候補・根本層）
- **★ /posts?max_created_at の nginxキャッシュ**（Reg試算 単独+48k・規定上安全か要確認＝コメント変化の影響）。
- template Execute削減（reflect.Call圧縮）、comment_count非正規化（COUNT排除）、/@user軽量化。
- ※400k可否(Reg): 同居2vCPU天井~390k、ベンチ別ホスト化は+4%（任意）。10倍差の主因はSW最適化の深さ＝到達可能。

---

## Step 12（不採用・revert）: コメント本体を ROW_NUMBER で各投稿最新3件に SQL 限定

### 試した施策
makePosts の !allComments 経路で、全コメントfetch→Go破棄を `ROW_NUMBER() OVER (PARTITION BY post_id ORDER BY created_at DESC, id DESC) <= 3` で SQL側3件限定に。Verifier は出力等価 PASS。

### 結果: **微減 204283 → 200860（-1.7%, fail0）→ revert**
- 再pprof: 狙い通り app側 scanAll 17%→7.9%（コメント無駄scan半減）。だが…
- **mysqld 50.5%→54.5% に上昇**（窓関数の PARTITION/sort コスト）。app 50.3→47.6%。
- ＝コストを app(余裕あり) から **mysqld(co-bottleneck) に移しただけで純減**。

### 学び（重要）
- **app と mysqld が 50/50 拮抗のときは、片側の負荷を減らしてもう片側に積む施策は逆効果**。スコアを上げるには「総量を減らす」か「より忙しい側を減らす」必要がある。
- html/template.Execute は 47.9%→48.4% で不変＝コメントscan削減では app 最大コスト(テンプレ描画)に届かない。
- → 正解は **両層から仕事を消すキャッシュ**（Step13: /posts nginx cache）。Step11(204283) を採用ベースに戻した。

---

## Step 13: /posts?max_created_at= の nginx proxy_cache — score 231,540 (warm中央値)

### 背景
Reg が benchmarker 実装でキャッシュ安全性を確認: /posts は `画像数≥20` のみ検証、max_created_at=**2016固定**でベンチ新規投稿(2026)は結果に入らない＝**返す投稿集合は初期データで不変**（正当に不変・gamingでない）。Step12の学び「両層から仕事を消す」に合致。

### 施策（tuning/STEP13_posts_cache.sh / nginx）
`proxy_cache_path /var/cache/nginx/posts ...` ＋ server に `location = /posts { proxy_cache posts_cache; proxy_cache_valid 200 24h; }`。X-Cacheヘッダで MISS→HIT 確認。アプリ・DB不変。

### Before → After（warm中央値）
| 指標 | Step 11 | Step 13 |
|---|---|---|
| score | 204283 | **231540 (+13.3%, +27k)** |
| fail | 0 | 0 |
| GET /posts avg | ~41ms | **0.00ms (HIT~100%)** |
| app(Go) %CPU | 50.3 | 46.6 |
| mysqld %CPU | 50.5 | 47.5 |

### 実効確認
X-Cache HIT率~100%（probe全HIT）。GET /posts は n=7040 で avg 0.00ms＝nginxが即返し app/DB を一切叩かない。fail/stale なし。

### 🎯 次のボトルネック: GET / レイテンシ（CPU遊休下の latency 律速）
- system合計 ~175%/200 ＝ **CPU約25%遊休**。app/mysqld とも ~47% で**未飽和**。
- ＝もはやCPU律速でなく、**11ワーカーが GET /（13.16ms・最多 n=15458）の応答待ち**で律速。GET/レイテンシを削れば遊休CPUを使ってスコアが伸びる。
- GET / の13ms = makePosts(効率化済) + **template Execute（pprof 48%, reflect.Value.Call 24%）**が主。GET/はlogin/CSRF/ban依存で素直なHTTPキャッシュ不可。

### 次（Step14候補）
- **GET / の template Execute 高速化**（code-gen テンプレ/reflection削減）＝GET/レイテンシ直撃。
- or GET / の匿名版キャッシュ+ログイン時動的注入（690k戦略としてReg精査中）。
- ※690k目標: Reg再分析中（現HWのSW天井 vs 増コア必要性）。

---

## Step 14: テンプレの reflect.Value.Call 排除（事前計算フィールド化） — score 229,814（横ばい・重要な診断）

### 背景・狙い
Step13後、CPU~25%遊休＝GET/(13.16ms・最多リク)のレイテンシ律速と判断。pprofで GET/ コストは template Execute 48%、うち **reflect.Value.Call 24%**。全テンプレ中 reflect.Value.Call を起こすのは post.html の3箇所のみ（投稿1件ごと発火＝GET/で20件×3）: `{{imageURL .}}`(FuncMap関数)・`{{.CreatedAt.Format ...}}`(メソッド×2)。他は全部フィールドアクセスか eq/if/range ビルトイン（Call不使用）。

### 施策（webapp/golang/）
- Post struct に `ImageURL string` / `CreatedAtFmt string` を追加。
- makePosts の投稿選択ループ（全描画経路の唯一の集約点）で `p.ImageURL = imageURL(p)` / `p.CreatedAtFmt = p.CreatedAt.Format(ISO8601Format)` を append 前に事前計算。
- templates/post.html の3箇所をフィールド参照に置換（`{{.ImageURL}}` / `{{.CreatedAtFmt}}`×2）。

### 検証（Verifier PASS）
3経路すべてバイト等価。html/template の文脈依存エスケープは「値の型(string)＋配置文脈」で決まり func戻り値かfieldかは無関係（src=URL文脈/data-created-at・datetime=属性文脈で同一escaper）。ISO8601Format=="2006-01-02T15:04:05-07:00" がpost.htmlリテラルと一致。post.htmlを含む4テンプレ(index/user/posts/postID)対応ハンドラは全て makePosts 経由。imageURL FuncMap未参照化は無害。go build/vet OK。

### Before → After（warm中央値, warmup破棄+本2回）
| 指標 | Step 13 | Step 14 |
|---|---|---|
| score | 231540 | 229814（**横ばい・-0.7%=ノイズ内**） |
| fail | 0 | 0 |
| GET / avg | 13.16ms | **12.55ms (-5%)** |
| GET /@user avg | 18.49ms | 17.50ms (-5%) |
| app(Go) %CPU | 46.6 | 45.6 (-1pt) |
| mysqld %CPU | 47.5 | 48.4 |

### 🎯 重要な診断: 「もはや app per-request CPU/レイテンシ律速ではない」
- 狙い通りGET/レイテンシ-5%・app CPU-1ptは**実現**したのに**スコアは不変**。
- 理由: success~219k/60s ＝ **~3650 req/s**。11固定workerで blended latency ≈ 3ms（多数のnginx静的/キャッシュ済/posts(0ms)応答で希釈）。GET/(12.5ms)は最多*回数*だが*総worker時間*では少数派 → GET/を5%削っても blended は ~0.3%しか動かず、run間変動(±3%, run1 233460/run2 226167)に埋もれる。
- CPUは app45.6+mysqld48.4+nginx21.3+bench32.3 ≈ 148%/200 で**~25%遊休のまま頭打ち**。
- ＝単機能のper-req CPU/レイテンシ削り(micro-opt)はもう費用対効果が逓減。次は「経路を丸ごと外す(キャッシュ)」か「HW(bench別ホスト/増コア)」の構造的レバー。

### 次（Step15: 構造的レバーの選択 — Reg精査中）
- 仮説(a): 同居benchmarker(32%CPU・固定11worker)のクライアント側律速 → bench別ホスト/増vCPUで一段上がる見込み（旧+2-4%見積もりはapp-CPU律速時のもの。遊休頭打ちの今は上振れ可能性）。
- 仮説(b): サーバ側残存律速（session memcache往復/MySQLレイテンシ/per-req固定費）。
- SW最大レバー: GET/(最多・非キャッシュ)を丸ごとキャッシュ。ただしGET/は新規投稿(2026)が先頭に来て不変でない→短TTL or fragmentキャッシュの安全性をReg確認中。

---

## Step 15: 匿名(未ログイン)GET/ の nginx proxy_cache — score 244,635（+14.8k）

### 背景・診断の確定
Step14のnull resultを受け、Benchが**負荷中のライブ計測**を実施→頭打ちの真因を確定:
- **`top` の負荷中 idle = 0.0% ＝ 2vCPU完全飽和**。Step14で記した「~25%遊休」は pidstat平均がrun間ギャップ/初期化/ランプを含んで薄まった**アーティファクト**で誤り（訂正）。
- **reqtime ≒ uptime（全エンドポイント差≈0）** ＝ accept/接続キュー/クライアント側の隠れ待ちは無く、観測レイテンシ＝サーバ処理そのもの。
- ＝micro-opt(Step14)でapp 1pt空けても、飽和した共有プール(mysqld~47%/bench~33%)が即吸収しスコア不変。**飽和下では「総CPU仕事量を減らす」=経路を丸ごと外すキャッシュが正解**。

### 施策（tuning/STEP15_index_cache.sh / nginxのみ）
Reg がbenchmarker実装で安全性を確認: benchmarker作者が `scenario.go:154`「トップページをキャッシュして超高速に返されたとき対策」と**GET/キャッシュを明示想定**（loadIndex 2-5回目はCheckFunc無し）。匿名GET/の検証は画像数≥20のみ（初期1万投稿で常に充足）。
- `proxy_cache_path .../index keys_zone=index_cache` ＋ `location = / { proxy_cache index_cache; proxy_cache_valid 200 60s; }`。
- **安全設計**: セッションCookie名は実コード = `isuconp-go.session`(app.go:137。Reg案の`isucon_session`は誤りで訂正)。Cookie有→`proxy_cache_bypass`/`proxy_no_cache`でGoへ素通し（ban確認/CSRF抽出/account-name表示を新鮮に保つ）。匿名GET/はSet-Cookieを出さない（getSessionUser/getCSRFTokenは読むだけSaveせず、getFlashはflash有時のみSave）→綺麗にキャッシュでき匿名workerがCookieを獲得せず以後も必ずHIT。

### 検証（Bench適用probe・全PASS）
- 4a 匿名(Cookieなし): 1st=MISS→2nd=**HIT** ✓
- 4b Cookieあり: **BYPASS**（認証経路は素通しでGoへ＝ban/CSRF検証が保たれfail回避）✓
- 4c 匿名GET/は **Set-Cookieなし** ✓

### Before → After（warm中央値, warmup破棄+本2回）
| 指標 | Step 14 | Step 15 |
|---|---|---|
| score | 229814 | **244635（+6.4%・+14.8k）** |
| fail | 0 | 0 |
| GET / avg | 12.55ms | **5.03ms (-60%)** |
| GET / cache HIT | — | 41%(匿名分・0ms) / 残59%は認証BYPASS |
| GET /@user avg | 17.50ms | 13.06ms |
| app(Go) %CPU | 45.6 | 44.2 |
| mysqld %CPU | 48.4 | ~47.5 |

### なぜ Reg試算+105k でなく +14.8k か
1. **キャッシュ対象は匿名GET/の41%のみ**。benchトラフィックはログイン済workerが多数で、認証GET/は安全上BYPASS→Goへ流れる。100%キャッシュ前提のReg試算と乖離。
2. **2vCPU飽和**のため、空いたapp CPUは飽和共有プール(mysqld/bench)に吸収されスループット増が逓減。

### 🎯 次のボトルネック（飽和下の最大レバー）
- **SW（新HW不要）: mysqld~47%のDB側CPU削減**が最大の残レバー（飽和の最大単一消費）。候補: ①getSessionUserの毎リクSELECT(認証全リク)をsession(memcache)格納で排除 ②comment COUNT(*)非正規化/省略(benchは件数未検証) ③残クエリ最適化。※まず mysqld のクエリダイジェスト実測で当て推量を避ける(Step10 pprofのDB版)。
- **HW（研修の本番ベンチ向け・ユーザー判断）: bench別ホスト化(+18〜42k) / 4vCPU化(690k圏)**。「マルチスレッド/マルチプロセス＝多コアスケール」はここで開花。事前に nginx `worker_processes auto`・Go `GOMAXPROCS`・GC削減(STW)を担保。
