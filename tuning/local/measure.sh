#!/bin/bash
# 発熱対策つき計測。usage: measure.sh <label> [per_run_cooldown_sec]
#   各「本計測run」の前に冷却+サーマル確認(pmset CPU_Speed_Limit=100まで待つ)を行う。
#   → 後半runの発熱スロットルによる下振れ(熱汚染)を防ぎ、各runを必ず冷えた状態で測る。
#   warmup1回(破棄) + 本計測3回。中央値を採用。
set -uo pipefail
LABEL="${1:-measure}"
PER="${2:-45}"   # 各run前の冷却(秒)。pmset監視が実スロットル時はさらに延長。
BIN=/Users/riku-ito/Workspaces/sandbox/private-isu/benchmarker/bin/benchmarker
UD=/Users/riku-ito/Workspaces/sandbox/private-isu/benchmarker/userdata
T=http://localhost

speedlimit() { pmset -g therm 2>/dev/null | grep -oE 'CPU_Speed_Limit *= *[0-9]+' | grep -oE '[0-9]+$' | tail -1; }

cooldown() {
  echo "[$LABEL] cooldown ${1}s + thermal wait ..."; sleep "$1"
  for _ in $(seq 1 30); do
    lim=$(speedlimit); lim=${lim:-100}
    echo "[$LABEL] CPU_Speed_Limit=${lim}"
    [ "$lim" -ge 100 ] && return
    echo "[$LABEL] throttled(${lim}) -> rest 30s"; sleep 30
  done
}

cooldown "$PER"
echo "[$LABEL] warmup (discard)"; "$BIN" -u "$UD" -t "$T" 2>/dev/null
for i in 1 2 3; do
  cooldown "$PER"           # 各run前に必ず冷却+サーマル確認(熱汚染防止)
  echo "[$LABEL] run $i:"; "$BIN" -u "$UD" -t "$T"
done
echo "[$LABEL] DONE"
