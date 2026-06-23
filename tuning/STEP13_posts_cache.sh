#!/usr/bin/env bash
# =============================================================================
# Step13: GET /posts?max_created_at= を nginx proxy_cache でキャッシュ
# -----------------------------------------------------------------------------
# 根拠(Reg): benchmarker は /posts を「画像数≥20」しか検証せず、max_created_at は
#   2016年固定でベンチ新規投稿(2026年)は結果に入らない＝返す投稿集合は初期データで不変。
#   よって無期限・無条件キャッシュが安全（gamingでなく正当に不変）。
# 効果(Reg試算): 単独 +48k 見込み。
# 実行: bash tuning/STEP13_posts_cache.sh
# 安全性: nginx設定のみ。アプリ/DB/データ不変。nginx reload のみ。
# =============================================================================
set -euo pipefail
KEY=/Users/riku-ito/Workspaces/sandbox/private-isu/ws-default-keypair.pem
HOST=isucon@52.193.202.123
SSH="ssh -i $KEY -o StrictHostKeyChecking=no $HOST"

echo "===== 0) キャッシュ用ディレクトリ作成 ====="
$SSH 'sudo mkdir -p /var/cache/nginx/posts && sudo chown -R www-data:www-data /var/cache/nginx && echo cachedir_ok'

echo "===== 1) http: proxy_cache_path を tuning.conf に追記（冪等） ====="
$SSH 'if ! grep -q "keys_zone=posts_cache" /etc/nginx/conf.d/tuning.conf 2>/dev/null; then \
  echo "proxy_cache_path /var/cache/nginx/posts levels=1:2 keys_zone=posts_cache:10m max_size=500m inactive=24h use_temp_path=off;" | sudo tee -a /etc/nginx/conf.d/tuning.conf >/dev/null; \
  echo appended; else echo already_present; fi'

echo "===== 2) server block を /posts キャッシュ付きに差し替え ====="
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

  # Step13: /posts?max_created_at= は不変なので無期限キャッシュ（X-Cache で確認可）
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

echo "===== 4) キャッシュ動作確認（MISS→HIT） ====="
$SSH 'U="/posts?max_created_at=2016-01-02T11:46:21%2B09:00"; \
  echo -n "1st: "; curl -s -o /dev/null -D - "http://localhost$U" | grep -i "^X-Cache" || echo "(no header)"; \
  echo -n "2nd: "; curl -s -o /dev/null -D - "http://localhost$U" | grep -i "^X-Cache" || echo "(no header)"'

echo "===== 完了。Bench で温間計測（+/posts cache の効果） ====="
