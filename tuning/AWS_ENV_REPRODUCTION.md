# private-isu AWS 環境再現 Runbook

作成日: 2026-06-23 / ベース: 実稼働 EC2（チューニング Step18 適用済み、score 270,092）

---

## 1. 概要

本ドキュメントは **private-isu チューニング環境**（ISUCON 練習用）を AWS 上で一から再現するための runbook である。  
対象は Go 実装（chi + sqlx + gomemcache）を使った以下の構成:

```
internet → nginx:80 → isu-go app:8080 → MySQL:3306
                                       → memcached:11211
```

初期 AMI（ami-09201e964bee13733）を起動し、アプリのビルドとチューニング設定を適用することで  
スコア 270,092（Step18 時点）相当の環境を再現できる。

---

## 2. インスタンス仕様（実測）

| 項目 | 値 |
|---|---|
| インスタンスタイプ | **c6i.large** |
| AMI ID | ami-09201e964bee13733 |
| リージョン | ap-northeast-1（東京）|
| AZ | ap-northeast-1a |
| vCPU | **2** |
| RAM | **3.7 GiB**（swap なし） |
| ディスク | 15 GB（root /）|
| 利用中 | 11 GB（77%）|
| 空き | 3.5 GB |
| /var/lib/mysql | 1.8 GB |
| /webapp/public/image | 1.6 GB（画像ファイル 約1万枚） |
| パブリック IP | 52.193.202.123 |
| プライベート IP | 192.168.1.10 |

> ⚠️ ディスクは 15 GB と小さく、チューニング進行（高スループット化）とともにベンチ書込み画像・binlog で逼迫しやすい。binlog 無効化（Step7）が必須。

---

## 3. OS・カーネル・導入ミドルとバージョン一覧（実測）

| コンポーネント | バージョン | 備考 |
|---|---|---|
| OS | Ubuntu 24.04.4 LTS (Noble Numbat) | |
| カーネル | 6.17.0-1015-aws | `uname -a` 実測 |
| Go (アプリバイナリ) | **1.26.3** | `/home/isucon/.local/go/bin/go`；バイナリ内埋め込みで確認 |
| nginx | **1.24.0** (Ubuntu) | `nginx -v` 実測 |
| MySQL | **8.0.45**-0ubuntu0.24.04.1 | `mysql --version` 実測 |
| memcached | **1.6.24** | `memcached -V` 実測 |
| Go 依存ライブラリ | go.mod 参照 | chi v5.3.0 / sqlx v1.4.0 / gomemcache / go-sql-driver/mysql v1.10.0 / gorilla-sessions |

### go.mod 抜粋
```
module github.com/catatsuy/private-isu/webapp/golang
go 1.24.0

require (
    github.com/bradfitz/gomemcache v0.0.0-20260422231931-4d751bb6e37c
    github.com/bradleypeabody/gorilla-sessions-memcache v0.0.0-20240916143655-c0e34fd2f304
    github.com/go-chi/chi/v5 v5.3.0
    github.com/go-sql-driver/mysql v1.10.0
    github.com/gorilla/sessions v1.4.0
    github.com/jmoiron/sqlx v1.4.0
)
```

---

## 4. ディレクトリ構成と稼働ユニット

### 主要ディレクトリ

```
/home/isucon/
├── env.sh                              # 環境変数（DB接続情報等）
├── .local/go/bin/go                    # Go toolchain（PATH未設定のため絶対パス必要）
├── private_isu/
│   ├── benchmarker/                    # ベンチマーカー（同一ホストで実行）
│   │   └── bin/benchmarker
│   ├── webapp/
│   │   ├── golang/                     # Go実装
│   │   │   ├── app                     # ビルド済みバイナリ（14.8MB）
│   │   │   ├── app.go                  # メインソース（Step18適用済み）
│   │   │   ├── go.mod / go.sum
│   │   │   ├── Makefile                # `go build -o app`
│   │   │   └── templates/              # HTMLテンプレート
│   │   ├── public/
│   │   │   └── image/                  # 投稿画像FS保存先（1.6GB / 約1万枚）
│   │   └── sql/
│   └── sql/
├── tuning/                             # サーバ上チューニングSQL記録
│   ├── 01_indexes.sql                  # Step1: comments/posts インデックス
│   └── 02_indexes.sql                  # Step5: comments(user_id) インデックス
```

### 稼働 systemd ユニット

全ユニット `active`（実測）:

| ユニット | ポート | ユーザー |
|---|---|---|
| isu-go | 127.0.0.1:8080 | isucon |
| nginx | 0.0.0.0:80 | www-data |
| mysql | 127.0.0.1:3306 | mysql |
| memcached | 127.0.0.1:11211 | memcache |

#### isu-go.service（`/etc/systemd/system/isu-go.service`）
```ini
[Unit]
Description=isu-go
After=syslog.target

[Service]
WorkingDirectory=/home/isucon/private_isu/webapp/golang
EnvironmentFile=/home/isucon/env.sh
Environment=RACK_ENV=production
PIDFile=/home/isucon/private_isu/webapp/golang/server.pid
User=isucon
Group=isucon
ExecStart=/home/isucon/private_isu/webapp/golang/app -bind "127.0.0.1:8080"
ExecStop=/bin/kill -s QUIT $MAINPID

[Install]
WantedBy=multi-user.target
```

> **GOMAXPROCS**: app.go に明示設定なし。`runtime.NumCPU()` 既定 = 2 vCPU。

#### nginx.service.d/limits.conf（`/etc/systemd/system/nginx.service.d/limits.conf`）
```ini
[Service]
LimitNOFILE=65535
```
> Step7b で追加。FD 1024 デフォルトでは高スループット時に `Too many open files` で崩落する。

---

## 5. 適用済みチューニング設定

### 5-1. MySQL インデックス（実測）

```
posts テーブル:
  PRIMARY (id)
  idx_user_id (user_id)           -- Step1 追加
  idx_created_at (created_at)     -- Step1 追加（FORCE INDEX 対象）

comments テーブル:
  PRIMARY (id)
  idx_post_created (post_id, created_at)  -- Step1 追加
  idx_user_id (user_id)                   -- Step5 追加

users テーブル:
  PRIMARY (id)
  account_name (UNIQUE)           -- 初期スキーマから存在
```

#### DDL（`tuning/01_indexes.sql`）
```sql
ALTER TABLE comments ADD INDEX idx_post_created (post_id, created_at);
ALTER TABLE posts ADD INDEX idx_user_id (user_id);
ALTER TABLE posts ADD INDEX idx_created_at (created_at);
```

#### DDL（`tuning/02_indexes.sql`）
```sql
ALTER TABLE comments ADD INDEX idx_user_id (user_id);
```

### 5-2. MySQL 設定（実測値）

| 変数 | 値 | 備考 |
|---|---|---|
| innodb_buffer_pool_size | **1,073,741,824（1GB）** | デフォルト 128MB から変更 |
| innodb_buffer_pool_instances | 8 | |
| innodb_flush_log_at_trx_commit | **2** | デフォルト 1 から変更（redo fsync を毎秒1回に緩和） |
| log_bin | **0 (OFF)** | disable_log_bin でバイナリログ無効化 |
| max_connections | 151 | |
| version | 8.0.45 | |

#### `/etc/mysql/mysql.conf.d/zz-tuning.cnf`（Drop-in。実機確認済み）
```ini
[mysqld]
innodb_buffer_pool_size = 1G
disable_log_bin
innodb_flush_log_at_trx_commit = 2
```
> 注: このファイルは既存 mysqld.cnf より後に読まれるよう `zz-` プレフィックスにする。

### 5-3. nginx 設定（実測：`sudo nginx -T` で確認）

#### `/etc/nginx/nginx.conf`（主要部）
```nginx
user www-data;
worker_processes auto;          # vCPU 数に応じて自動（実測 2 workers）
worker_rlimit_nofile 65535;     # Step7b: FD 上限引き上げ
pid /run/nginx.pid;

events {
    worker_connections 8192;    # Step7b: デフォルト 768 から変更
}
```

#### `/etc/nginx/conf.d/ltsv_log.conf`
```nginx
log_format ltsv "time:$time_iso8601\tstatus:$status\tsize:$body_bytes_sent\treqtime:$request_time\tuptime:$upstream_response_time\tmethod:$request_method\turi:$uri";
```
> alp 等での集計用 LTSV ログ形式。`access_ltsv.log` に出力。

#### `/etc/nginx/conf.d/tuning.conf`
```nginx
open_file_cache max=10000 inactive=60s;
open_file_cache_valid 60s;
open_file_cache_min_uses 1;
open_file_cache_errors off;
proxy_cache_path /var/cache/nginx/posts levels=1:2 keys_zone=posts_cache:10m max_size=500m inactive=24h use_temp_path=off;
proxy_cache_path /var/cache/nginx/index levels=1:2 keys_zone=index_cache:10m max_size=100m inactive=120s use_temp_path=off;
```

#### `/etc/nginx/sites-available/isucon.conf`（Step15 最終版・実機確認済み）
```nginx
upstream app {
  server 127.0.0.1:8080;
  keepalive 64;                 # Step7: Goへの接続をHTTP/1.1で再利用
}

server {
  listen 80;
  client_max_body_size 10m;
  root /home/isucon/private_isu/webapp/public/;
  access_log /var/log/nginx/access_ltsv.log ltsv;

  gzip on;
  gzip_types text/css application/javascript application/json text/plain;
  gzip_min_length 1024;
  gzip_vary on;

  # Step15: 匿名(未ログイン)トップページのキャッシュ。Cookie有はGoへ素通し。
  location = / {
    set $bypass 0;
    if ($http_cookie ~* "isuconp-go\.session") {
      set $bypass 1;
    }
    proxy_cache index_cache;
    proxy_cache_key "$scheme$request_method$host$request_uri";
    proxy_cache_valid 200 60s;
    proxy_cache_bypass $bypass;
    proxy_no_cache $bypass;
    proxy_cache_lock on;
    proxy_ignore_headers Cache-Control Expires;
    add_header X-Cache $upstream_cache_status;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_pass http://app;
  }

  # Step13: /posts?max_created_at= は不変なので無期限キャッシュ
  location = /posts {
    proxy_cache posts_cache;
    proxy_cache_key "$scheme$request_method$host$request_uri";
    proxy_cache_valid 200 24h;
    proxy_cache_lock on;
    proxy_ignore_headers Set-Cookie Cache-Control Expires;
    add_header X-Cache $upstream_cache_status;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_pass http://app;
  }

  # Step7: 静的アセットは nginx 直配信。expiresで304加点。Go負荷オフロード。
  location ~* \.(css|js|ico|gif|png|jpg|jpeg|svg|woff2?)$ {
    expires 1d;
    access_log off;
    try_files $uri @app;
  }

  # Step3: 投稿画像はFS直配信（無ければGoへフォールバック）
  location /image/ {
    expires 1d;
    try_files $uri @app;
  }

  location @app {
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_pass http://app;
  }

  location / {
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_pass http://app;
  }
}
```

### 5-4. アプリ（Go）チューニング箇所（app.go）

| 施策 | 内容 | Step |
|---|---|---|
| パスワードハッシュ | `openssl` 外部プロセス → `crypto/sha512` 内製化 | Step2 |
| 画像 FS 保存 | 投稿時に `public/image/{id}.{ext}` へ書込み（DB BLOB は空） | Step3/16 |
| タイムラインクエリ | `STRAIGHT_JOIN + FORCE INDEX(idx_created_at)`、LIMIT 20 | Step5/6 |
| DSN | `interpolateParams=true`（prepare+exec 2往復→1往復） | Step8 |
| 接続プール | `SetMaxOpenConns(100)`, `SetMaxIdleConns(100)`, `SetConnMaxLifetime(0)` | Step8 |
| テンプレートパース | 起動時1回パース（ハンドラ毎の `ParseFiles` 廃止） | Step9 |
| ユーザーIN絞り | `SELECT ... FROM users WHERE id IN(?)` で必要ユーザーのみ取得 | Step11 |
| 投稿画像 DB BLOB | INSERT posts の imgdata を `[]byte{}`（空）に → DB 書込み廃止 | Step16 |
| users プロセス内キャッシュ | `sync.RWMutex` + `map[int]User`。/initialize 全クリア、ban 時 id 削除 | Step17 |
| COUNT クエリ排除 | コメント件数をコメント本体取得結果から Go 側でカウント | Step18 |
| pprof | `localhost:6060`（外部非公開）で CPU プロファイル採取 | Step10 |

---

## 6. 環境変数

### `/home/isucon/env.sh`（実測）
```bash
PATH=/usr/local/bin:/home/isucon/.local/ruby/bin:/home/isucon/.local/node/bin: \
    /home/isucon/.local/python3/bin:/home/isucon/.local/perl/bin: \
    /home/isucon/.local/php/bin:/home/isucon/.local/php/sbin: \
    /home/isucon/.local/go/bin:/home/isucon/.local/scala/bin:/usr/bin/:/bin/:$PATH
ISUCONP_DB_USER=isuconp
ISUCONP_DB_PASSWORD=isuconp
ISUCONP_DB_NAME=isuconp
```

> **注**: `ISUCONP_DB_HOST` / `ISUCONP_DB_PORT` は未設定（→ app.go デフォルト: `127.0.0.1:3306`）。  
> `ISUCONP_MEMCACHED_ADDRESS` は未設定（→ app.go デフォルト: `localhost:11211`）。

### isu-go の systemd 追加 Environment
```
RACK_ENV=production
```

---

## 7. ネットワーク / セキュリティグループ / ポート

### 開放ポート（実測: `sudo ss -tlnp`）

| ポート | バインド | プロセス | 備考 |
|---|---|---|---|
| 22 (SSH) | 0.0.0.0 / [::] | sshd | SG: SSH 許可元 IP = 158.95.54.208（Netskope 経由出口）|
| 80 (HTTP) | 0.0.0.0 | nginx | SG: ベンチマーカー IP を許可 |
| 3306 (MySQL) | 127.0.0.1 | mysqld | ローカルのみ。SG 不要 |
| 8080 (app) | * | app (Go) | ローカルのみ（nginx → app proxy）。SG 不要 |
| 11211 (memcached) | 127.0.0.1 / [::1] | memcached | ローカルのみ |
| 6060 (pprof) | 127.0.0.1 | app (Go) | pprof エンドポイント。外部非公開 |

### SG ルール（判明範囲・実体は AWS コンソール参照）
- **Inbound TCP 22**: 158.95.54.208/32（Netskope VPN 出口 IP。変動する可能性あり）
- **Inbound TCP 80**: ベンチマーカーホスト IP（本環境では同一 EC2 から実行のため localhost）
- **Outbound**: 全許可（ALB 不使用・直 EC2 構成）

---

## 8. ゼロから再現する手順

### 前提
- AWS マネジメントコンソール or CLI でインスタンスを起動できる
- SSH 鍵ペア（ws-default-keypair.pem）が手元にある
- ローカルに本リポジトリをクローン済み（`private-isu/`）

---

### Step A: インスタンス起動

```bash
# AP-NE-1a / c6i.large / Ubuntu 24.04 / AMI: ami-09201e964bee13733
# セキュリティグループ: TCP 22（自端末 IP）、TCP 80（オープン or ベンチ元 IP）
# ストレージ: gp3 15GB 以上（binlog 無効化前は肥大するので 20GB 推奨）
# キーペア: ws-default-keypair.pem
```

SSH 接続確認:
```bash
ssh -i ws-default-keypair.pem -o StrictHostKeyChecking=no isucon@<PUBLIC_IP>
```

---

### Step B: OS セットアップ（AMI 起動後・初回のみ）

AMI にミドルウェアは導入済みのため追加インストール不要。  
Go toolchain は `/home/isucon/.local/go/bin/go` に配置済み。

```bash
# Go が PATH に入っていない場合
echo 'export PATH=/home/isucon/.local/go/bin:$PATH' >> ~/.bashrc
source ~/.bashrc
go version  # go1.26.3 を確認
```

---

### Step C: リポジトリ取得と初期化

```bash
# サーバ上でリポジトリが既にある場合はスキップ
ls /home/isucon/private_isu/  # 存在確認

# なければ clone（AMI 起動時は既にある想定）
# git clone https://github.com/catatsuy/private-isu.git /home/isucon/private_isu

# 初期データロード（DBダンプ・画像）
cd /home/isucon/private_isu
make init  # dump.sql.bz2 と画像をダウンロード・展開
```

---

### Step D: データベース初期化とインデックス追加

```bash
# DB 初期化（dump.sql ロード）
# AMI のDB が既にある場合は不要。クリーンな場合のみ:
mysql isuconp < /home/isucon/private_isu/webapp/sql/dump.sql

# インデックス追加（Step1 + Step5）
mysql -u isuconp -pisuconp isuconp < /home/isucon/tuning/01_indexes.sql
mysql -u isuconp -pisuconp isuconp < /home/isucon/tuning/02_indexes.sql

# 確認
mysql -u isuconp -pisuconp isuconp -e "SHOW INDEX FROM posts; SHOW INDEX FROM comments;"
```

---

### Step E: MySQL チューニング設定適用（Step7）

```bash
# Drop-in 設定ファイル配置
sudo tee /etc/mysql/mysql.conf.d/zz-tuning.cnf > /dev/null <<'EOF'
[mysqld]
innodb_buffer_pool_size = 1G
disable_log_bin
innodb_flush_log_at_trx_commit = 2
EOF

# binlog 既存ファイルを先に PURGE（ディスク逼迫防止）
sudo mysql -e "PURGE BINARY LOGS BEFORE NOW(6)"

# MySQL 再起動で設定反映
sudo systemctl restart mysql

# 確認
mysql -u isuconp -pisuconp isuconp -e \
  "SELECT @@innodb_buffer_pool_size, @@log_bin, @@innodb_flush_log_at_trx_commit;"
# 期待値: 1073741824 | 0 | 2
```

---

### Step F: nginx チューニング設定適用（Step7 + Step7b + Step13 + Step15）

各スクリプトをローカルから実行（SSH over から適用）:

```bash
# ローカルから（public IP を設定して実行）
PUBLIC_IP=52.193.202.123  # 新 IP に変更
sed -i "s/52\.193\.202\.123/$PUBLIC_IP/" tuning/STEP7_apply.sh
bash tuning/STEP7_apply.sh        # Step7: MySQL/nginx設定一括適用
bash tuning/STEP7b_nginx_fd.sh    # Step7b: FD上限引き上げ
bash tuning/STEP13_posts_cache.sh # Step13: /posts キャッシュ
bash tuning/STEP15_index_cache.sh # Step15: 匿名GET/ キャッシュ
```

または手動でサーバ上に設定ファイルを配置して `sudo systemctl restart nginx`。  
（設定内容は上記セクション 5-3 を参照）

nginx proxy_cache ディレクトリの権限確認:
```bash
sudo mkdir -p /var/cache/nginx/posts /var/cache/nginx/index
sudo chown -R www-data:www-data /var/cache/nginx
```

---

### Step G: アプリ（Go）ビルドとデプロイ

ローカルの最新 app.go をサーバに転送してビルド:

```bash
# ローカルから
ssh -i ws-default-keypair.pem isucon@$PUBLIC_IP 'cat > /home/isucon/private_isu/webapp/golang/app.go' \
  < webapp/golang/app.go

# サーバでビルド
ssh -i ws-default-keypair.pem isucon@$PUBLIC_IP '
  source ~/.bashrc
  export PATH=/home/isucon/.local/go/bin:$PATH
  cd /home/isucon/private_isu/webapp/golang
  go build -o app && echo "build OK"
'
```

---

### Step H: 画像ファイルの初期FS出力（Step3）

DBに保存されている初期画像（9,814件）をFSに書き出す:

```bash
# cmd/dumpimages があれば使用
ssh -i ws-default-keypair.pem isucon@$PUBLIC_IP '
  export PATH=/home/isucon/.local/go/bin:$PATH
  cd /home/isucon/private_isu/webapp/golang
  # ヘルパツール（dumpimages）でDB BLOBを一括FS書き出し
  go run cmd/dumpimages/main.go
  ls public/image/ | wc -l  # 約9814件確認
'
```

---

### Step I: サービス起動と動作確認

```bash
# サービス起動
sudo systemctl restart isu-go
sudo systemctl reload nginx  # nginx は設定変更時のみ restart（FD上限はrestartが必要）

# 動作確認
systemctl is-active isu-go nginx mysql memcached
curl -s -o /dev/null -w "%{http_code}\n" http://localhost/
# 200 が返ること
```

---

### Step J: ベンチマーク実行

```bash
cd /home/isucon/private_isu/benchmarker
./bin/benchmarker -u ./userdata -t http://localhost
# 期待スコア（Step18 warm中央値）: ~270,092
# {"pass":true,"score":270092,"success":...,"fail":0}
```

計測プロトコル: **ウォームアップ1回（破棄）＋本計測2回の中央値**。単発は variance が大きい。

---

## 9. 既知の注意点

### ① ディスク逼迫（最重要）

- ベンチマーカーが POSTする画像が `/home/isucon/private_isu/webapp/public/image/` に蓄積。  
  `/initialize` ハンドラが `id > 10000` の画像を削除するので、ベンチ後はファイル数 ~10000 で頭打ち。
- binlog（デフォルト ON）が毎回数十本生成される。高スループット化でPOSTが増えると急速に肥大。  
  → **Step7 で `disable_log_bin` を適用することが必須**。未適用の場合、スコア 3倍時点でディスク 100% になる（実体験）。
- ディスク解放の緊急手順:
  ```bash
  sudo truncate -s 0 /var/log/nginx/access_ltsv.log  # 最大116MB即時解放
  sudo mysql -e "PURGE BINARY LOGS BEFORE NOW(6)"
  df -h /
  ```

### ② nginx FD 上限（"Too many open files"）

- 静的配信＋高スループットで nginx が FD 1024 上限（OS デフォルト）に達すると  
  全接続を accept できなくなりスコアが 132k → 38k に崩落する（実体験、Step7b で発生）。
- 対処: `worker_rlimit_nofile 65535`（nginx.conf）+ systemd `LimitNOFILE=65535`。
- **設定反映には `nginx reload` ではなく `nginx restart` が必要**（FD 上限は reload では反映されない）。

### ③ 画像 FS 化の初期化整合

- `/initialize` ハンドラは DB の `posts` テーブルを `id > 10000` で削除する。  
  これに対応する画像ファイルも削除しないと FS が肥大し続ける。  
  `cleanupImageFiles()` を `/initialize` ハンドラに追加済み（Step4）。再実装時も必須。

### ④ 接続プール（`SetMaxOpenConns`）とインデックスの依存関係

- `FORCE INDEX(idx_created_at)` はそのインデックスが存在しないと 500 エラーになる（フォールバックなし）。  
  インデックスの適用（Step D）より前にアプリを起動すると本番同様の問題が起きる。  
  再現順序: **インデックス追加 → アプリビルド → アプリ起動**。

### ⑤ ベンチマーカーの同居（CPU 競合）

- 現構成ではベンチマーカーを同一 EC2（2 vCPU）上で実行しており、ベンチ自体が 30–35% CPU を消費する。  
  2 vCPU 完全飽和（idle 0%）のため、ソフトウェア施策の限界スコアは ~300k 程度。  
  **真の 400k+ を目指す場合はベンチマーカーを別ホストで実行するか、4 vCPU 以上のインスタンスが必要**。

### ⑥ users キャッシュと ban 整合

- Step17 で users をプロセス内キャッシュ化。ban 操作後の整合は二重防御:  
  ① タイムライン/プロフィール SQL が `WHERE del_flg=0` で DB 権威  
  ② `/admin/banned` でキャッシュから ban 対象 id を削除（次アクセスで DB 再読み）  
  ③ `/initialize` でキャッシュ全クリア

### ⑦ comments COUNT クエリ排除の留意点（Step18）

- `makePosts` のコメント件数をコメント本体取得の全件カウントで代替している。  
  **コメント本体クエリに SQL 側 LIMIT を追加すると CommentCount が壊れる**。  
  将来 ROW_NUMBER 等の絞り込みを入れる場合は COUNT クエリを復活させること。

---

## 付録: スコア推移サマリ

| Step | 主な施策 | スコア |
|---|---|---|
| 0 | Go ベースライン（インデックスなし） | 0 |
| 1 | インデックス追加（comments/posts） | 16,028 |
| 2 | パスワードハッシュ内製化 | 17,891 |
| 3 | 画像 FS 化 + nginx 静的配信 | 29,764 |
| 4 | makePosts N+1 解消 + /initialize 画像クリーンアップ | 31,085 |
| 5 | タイムラインクエリ LIMIT20 / imgdata 除外 / comments(user_id) idx | 43,763 |
| 6 | FORCE INDEX + STRAIGHT_JOIN（filesort 排除） | ~132,000 |
| 7 | binlog OFF + MySQL 設定 + nginx 静的/gzip/keepalive + FD 上限 | 156,127 |
| 8 | DB 接続プール + interpolateParams | 170,721 |
| 9 | テンプレート起動時1回パース | 173,720 |
| 10-11 | pprof 診断 + makePosts 全ユーザー走査 → IN 絞り | 204,283 |
| 13 | /posts?max_created_at の nginx キャッシュ | 231,540 |
| 14 | template reflect.Value.Call 排除（事前計算フィールド） | 229,814 |
| 15 | 匿名 GET/ の nginx キャッシュ（Cookie 有は bypass） | 244,635 |
| 16 | POST/ の画像 BLOB DB 書込み廃止（FS 保存のみ） | 258,645 |
| 17 | users プロセス内キャッシュ | 268,322 |
| **18** | **makePosts の冗長 COUNT クエリ排除** | **270,092** |

詳細は `tuning/TUNING_LOG.md` を参照。
