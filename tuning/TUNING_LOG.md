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
