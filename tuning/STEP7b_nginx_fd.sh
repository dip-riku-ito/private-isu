#!/usr/bin/env bash
# =============================================================================
# Step7b: nginx の FD/接続上限 引き上げ
# -----------------------------------------------------------------------------
# 背景: Step7適用後、高スループット+静的直配信で nginx が "Too many open files"(FD=1024)
#       となり accept 失敗→全接続timeout→スコア崩落(132k→38k)。性能ではなくFD上限が原因。
# 対処: systemd LimitNOFILE=65535 + nginx worker_rlimit_nofile 65535 + worker_connections 8192。
# 実行: bash tuning/STEP7b_nginx_fd.sh
# 安全性: nginx設定のみ。データ/MySQL不変。restart は nginx のみ。
# =============================================================================
set -euo pipefail
KEY=/Users/riku-ito/Workspaces/sandbox/private-isu/ws-default-keypair.pem
HOST=isucon@52.193.202.123
SSH="ssh -i $KEY -o StrictHostKeyChecking=no $HOST"

echo "===== 0) 現状のFD/接続上限 ====="
$SSH 'grep -nE "^worker_processes|^worker_rlimit_nofile|worker_connections" /etc/nginx/nginx.conf || true; \
      systemctl show nginx -p LimitNOFILE'

echo "===== 1) systemd: nginx の LimitNOFILE=65535 ====="
$SSH 'sudo mkdir -p /etc/systemd/system/nginx.service.d'
$SSH 'sudo tee /etc/systemd/system/nginx.service.d/limits.conf >/dev/null' <<'LIM'
[Service]
LimitNOFILE=65535
LIM
$SSH 'sudo systemctl daemon-reload && echo daemon-reloaded'

echo "===== 2) nginx.conf: worker_rlimit_nofile(main) + worker_connections(events) ====="
$SSH 'set -e; \
  if ! grep -q "^worker_rlimit_nofile" /etc/nginx/nginx.conf; then \
    sudo sed -i "/^worker_processes/a worker_rlimit_nofile 65535;" /etc/nginx/nginx.conf; \
  else \
    sudo sed -i "s/^worker_rlimit_nofile.*/worker_rlimit_nofile 65535;/" /etc/nginx/nginx.conf; \
  fi; \
  sudo sed -i "s/worker_connections[[:space:]]*[0-9]*;/worker_connections 8192;/" /etc/nginx/nginx.conf; \
  echo "--- after ---"; grep -nE "^worker_processes|^worker_rlimit_nofile|worker_connections" /etc/nginx/nginx.conf'

echo "===== 3) nginx 構文チェック ====="
$SSH 'sudo nginx -t'

echo "===== 4) nginx restart（FD上限はreloadでなくrestartで反映） ====="
$SSH 'sudo systemctl restart nginx && echo nginx-restarted'

echo "===== 5) 反映確認（workerの実FD上限 / 接続 / 応答） ====="
$SSH 'systemctl show nginx -p LimitNOFILE; \
      PID=$(pgrep -f "nginx: worker" | head -1); echo "worker pid=$PID"; \
      sudo cat /proc/$PID/limits 2>/dev/null | grep -i "open files" || true; \
      echo -n "nginx active: "; systemctl is-active nginx; \
      curl -s -o /dev/null -w "GET / => %{http_code}\n" http://localhost/'

echo "===== 完了。Step7+7b の本来効果を再計測へ ====="
