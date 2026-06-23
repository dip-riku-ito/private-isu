#!/bin/bash
# ベンチ実行。計測プロトコル: ウォームアップ1回(破棄) + 本計測3回。
# usage: tuning/local/bench.sh [target]   (default: http://localhost)
set -uo pipefail
T="${1:-http://localhost}"
BIN="/Users/riku-ito/Workspaces/sandbox/private-isu/benchmarker/bin/benchmarker"
UD="/Users/riku-ito/Workspaces/sandbox/private-isu/benchmarker/userdata"

echo "target=$T"
echo "== warmup (破棄) =="
"$BIN" -u "$UD" -t "$T" 2>/dev/null || true
for i in 1 2 3; do
  echo "== run $i =="
  "$BIN" -u "$UD" -t "$T"
done
