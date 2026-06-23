#!/bin/bash
# nginx設定の interleaved A/B 計測。各ペアで BASE→TREAT を連続測定し、マシンのドリフトをペア内で相殺する。
# nginx設定の差し替えは reload(~1s)のみ＝per-run交互が現実的。
# usage: measure_ab.sh <base.conf> <treat.conf> <pairs> <per_cooldown_sec>
set -uo pipefail
ROOT=/Users/riku-ito/Workspaces/sandbox/private-isu
P=isu-tuned
F=tuning/local/compose.tuned.yml
CONF="$ROOT/tuning/local/nginx-conf.d/isu.conf"
BASEF="${1:-$ROOT/tuning/local/nginx-variants/base.conf}"
TREATF="${2:-$ROOT/tuning/local/nginx-variants/gzipoff.conf}"
PAIRS="${3:-3}"
PER="${4:-30}"
BIN="$ROOT/benchmarker/bin/benchmarker"
UD="$ROOT/benchmarker/userdata"
T=http://localhost
cd "$ROOT"

speedlimit() { pmset -g therm 2>/dev/null | grep -oE 'CPU_Speed_Limit *= *[0-9]+' | grep -oE '[0-9]+$' | tail -1; }
cooldown() {
  sleep "$PER"
  for _ in $(seq 1 30); do
    lim=$(speedlimit); lim=${lim:-100}
    [ "$lim" -ge 100 ] && return
    echo "  throttled(${lim}) -> rest 30s"; sleep 30
  done
}
reload() { cp "$1" "$CONF"; docker compose -p "$P" -f "$F" exec -T nginx nginx -s reload >/dev/null 2>&1; }
run1()  { "$BIN" -u "$UD" -t "$T" 2>/dev/null; }

echo "AB: BASE=$(basename "$BASEF") vs TREAT=$(basename "$TREATF") / pairs=$PAIRS per=${PER}s"
reload "$BASEF"; cooldown; echo "warmup(discard):"; run1 >/dev/null
for i in $(seq 1 "$PAIRS"); do
  reload "$BASEF";  cooldown; printf 'BASE  p%s: '  "$i"; run1
  reload "$TREATF"; cooldown; printf 'TREAT p%s: ' "$i"; run1
done
reload "$BASEF"   # 終了時はBASEへ戻す
echo "AB DONE (nginx config restored to BASE)"
