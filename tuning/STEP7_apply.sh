#!/usr/bin/env bash
# =============================================================================
# Step7 適用 runbook: ディスク恒久解放 + nginx改善 + MySQL設定 を EC2 に適用
# -----------------------------------------------------------------------------
# 実行（ローカルから / sudo over ssh のため対話実行推奨）:
#     bash tuning/STEP7_apply.sh
# あるいは各セクションを手で実行してもよい。
#
# 安全性:
#   - 削除するのは破棄可能な binlog と肥大した nginx ログのみ。
#   - テーブル/データ(posts.imgdata カラム)は変更しない（imgdata DROP は別Stepで要確認）。
#   - 失敗時はその場で停止（set -e）。
# 前提: isucon ユーザに NOPASSWD sudo（無い場合は各 sudo を対話実行）。
# =============================================================================
set -euo pipefail

KEY=/Users/riku-ito/Workspaces/sandbox/private-isu/ws-default-keypair.pem
HOST=isucon@52.193.202.123
SSH="ssh -i $KEY -o StrictHostKeyChecking=no $HOST"

echo "===== 0) 現状確認 ====="
$SSH 'df -h /; mysql -V; nginx -v'

echo "===== 1) ディスク解放（先にログ切詰=即116MB解放 → binlog purge） ====="
# disk 100%(残28M)のため、まず巨大nginxログを切り詰めてMySQLに余裕を作ってから purge する。
$SSH 'sudo truncate -s 0 /var/log/nginx/access_ltsv.log 2>/dev/null && echo "nginx ltsv log truncated"; \
      sudo truncate -s 0 /var/log/nginx/access.log 2>/dev/null; \
      df -h / | tail -1; \
      sudo mysql -e "PURGE BINARY LOGS BEFORE NOW(6)" && echo "binlog purged"; \
      df -h /'

echo "===== 2) MySQL設定ドロップイン配置 (/etc/mysql/mysql.conf.d/zz-tuning.cnf) ====="
$SSH 'sudo tee /etc/mysql/mysql.conf.d/zz-tuning.cnf >/dev/null' <<'CNF'
[mysqld]
innodb_buffer_pool_size = 1G
disable_log_bin
innodb_flush_log_at_trx_commit = 2
CNF
echo "  -> 配置完了"

echo "===== 3) nginx: http チューニング (/etc/nginx/conf.d/tuning.conf) ====="
# 注: sendfile/tcp_nopush は Ubuntu既定の nginx.conf に既にあるため重複させない。open_file_cache のみ追加。
$SSH 'sudo tee /etc/nginx/conf.d/tuning.conf >/dev/null' <<'NGT'
open_file_cache max=10000 inactive=60s;
open_file_cache_valid 60s;
open_file_cache_min_uses 1;
open_file_cache_errors off;
NGT
echo "  -> 配置完了"

echo "===== 4) nginx: server block 差し替え (/etc/nginx/sites-available/isucon.conf) ====="
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

echo "===== 5) nginx 構文チェック ====="
$SSH 'sudo nginx -t'

echo "===== 6) 反映 ====="
# (A) サービス個別再起動（既定・速い・SSH維持）:
$SSH 'sudo systemctl restart mysql && sudo systemctl reload nginx && sudo systemctl restart isu-go'
# (B) EC2フル再起動を希望する場合は上の(A)をコメントアウトし、次行を有効化（SSHは切れ数十秒後に自動復帰）:
# $SSH 'sudo reboot' || true

echo "===== 7) 復帰確認 ====="
$SSH 'df -h /; \
      echo -n "services: "; systemctl is-active mysql nginx isu-go | tr "\n" " "; echo; \
      mysql -e "SELECT @@innodb_buffer_pool_size AS buffer_pool, @@log_bin AS log_bin, @@innodb_flush_log_at_trx_commit AS flush_commit"; \
      echo -n "GET / status: "; curl -s -o /dev/null -w "%{http_code}\n" localhost/'

echo "===== 完了。次は温間ベンチ計測（warm破棄+3回中央値, fail0確認） ====="
