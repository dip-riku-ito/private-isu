#!/usr/bin/env bash
# =============================================================================
# Step15: 匿名(未ログイン) GET / を nginx proxy_cache でキャッシュ
# -----------------------------------------------------------------------------
# 根拠(Reg):
#  - benchmarker は GET/ のキャッシュ化を「明示的に想定」して設計（scenario.go:154
#    「トップページをキャッシュして超高速に返されたとき対策」: loadIndex の2〜5回目は
#    CheckFunc無しで無条件+1pt）。
#  - 匿名GET/ の検証は imagePerPageChecker(画像数≥20) のみ＝初期1万投稿で常に充足、
#    新規投稿(2026)が先頭に来ても≥20は不変 → キャッシュ安全。
#  - logout後の匿名GET/ は .isu-account-name=="" を確認するが、匿名キャッシュは
#    ヘッダにaccount-nameを含まない（ログインリンク表示）ので空欄一致。✅
#
# 安全設計（重要）:
#  - セッションCookie名は Go実装 = "isuconp-go.session"（app.go:137 store.Get(r,"isuconp-go.session")）。
#    Cookieがあれば必ず Go へ素通し（ban確認/CSRF抽出/isu-account-name表示が必要なため）。
#  - 匿名GET/ は Set-Cookie を出さない（getSessionUser/getCSRFTokenは読むだけSaveしない、
#    getFlashはflashがある時だけSave＝匿名は無Save）→ レスポンスは綺麗にキャッシュ可能、
#    かつ匿名workerがCookieを獲得しない→以後も必ずキャッシュHIT。
#  - proxy_ignore_headers に Set-Cookie は入れない: 万一Set-Cookie付きでもnginxは
#    キャッシュせず動的フォールバック（安全側に倒す）。
#
# 効果(Reg試算): 単独 +105k 見込み（231.5k→~337k）。
# 実行: bash tuning/STEP15_index_cache.sh
# 安全性: nginx設定のみ。アプリ/DB/データ不変。nginx reload のみ。
# =============================================================================
set -euo pipefail
KEY=/Users/riku-ito/Workspaces/sandbox/private-isu/ws-default-keypair.pem
HOST=isucon@52.193.202.123
SSH="ssh -i $KEY -o StrictHostKeyChecking=no $HOST"

echo "===== 0) index キャッシュ用ディレクトリ作成 ====="
$SSH 'sudo mkdir -p /var/cache/nginx/index && sudo chown -R www-data:www-data /var/cache/nginx && echo cachedir_ok'

echo "===== 1) http: proxy_cache_path(index_cache) を tuning.conf に追記（冪等） ====="
$SSH 'if ! grep -q "keys_zone=index_cache" /etc/nginx/conf.d/tuning.conf 2>/dev/null; then \
  echo "proxy_cache_path /var/cache/nginx/index levels=1:2 keys_zone=index_cache:10m max_size=100m inactive=120s use_temp_path=off;" | sudo tee -a /etc/nginx/conf.d/tuning.conf >/dev/null; \
  echo appended; else echo already_present; fi'

echo "===== 2) server block を /posts + / 両キャッシュ付きに差し替え ====="
$SSH 'sudo tee /etc/nginx/sites-available/isucon.conf >/dev/null' <<'NGS'
upstream app {
  server 127.0.0.1:8080;
  keepalive 64;
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

  # Step15: 匿名(未ログイン)のトップページをキャッシュ。
  # Cookie(isuconp-go.session)があれば必ずGoへ（ban確認/CSRF/account-name表示のため）。
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

  location ~* \.(css|js|ico|gif|png|jpg|jpeg|svg|woff2?)$ {
    expires 1d;
    access_log off;
    try_files $uri @app;
  }
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
NGS
echo "  -> 差し替え完了"

echo "===== 3) nginx 構文チェック & reload ====="
$SSH 'sudo nginx -t && sudo systemctl reload nginx && echo nginx_reloaded'

echo "===== 4) キャッシュ動作確認 ====="
echo "--- 4a) 匿名(Cookieなし): MISS→HIT になるべき ---"
$SSH 'echo -n "1st: "; curl -s -o /dev/null -D - "http://localhost/" | grep -i "^X-Cache" || echo "(no header)"; \
  echo -n "2nd: "; curl -s -o /dev/null -D - "http://localhost/" | grep -i "^X-Cache" || echo "(no header)"'
echo "--- 4b) Cookieあり(認証想定): 常に BYPASS になるべき ---"
$SSH 'curl -s -o /dev/null -D - -H "Cookie: isuconp-go.session=dummy" "http://localhost/" | grep -i "^X-Cache" || echo "(no header / bypass)"'
echo "--- 4c) 匿名GET/ が Set-Cookie を出していないこと（出ていたらキャッシュ非対象に倒れる）---"
$SSH 'curl -s -o /dev/null -D - "http://localhost/" | grep -i "^Set-Cookie" && echo "WARN: Set-Cookie present" || echo "ok_no_set_cookie"'

echo "===== 完了。Bench で温間計測（GET/ anonymous cache の効果） ====="
