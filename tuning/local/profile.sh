#!/bin/bash
# 発熱対策つき: cooldown -> 負荷をかけながらCPU/heapプロファイル採取 -> 休息 -> rested baseline score
# 出力プロファイルreportは $OUT に保存(ideationチーム/synthがReadする)。
set -uo pipefail
BIN=/Users/riku-ito/Workspaces/sandbox/private-isu/benchmarker/bin/benchmarker
UD=/Users/riku-ito/Workspaces/sandbox/private-isu/benchmarker/userdata
SERVER=/Users/riku-ito/Workspaces/sandbox/private-isu/webapp/golang/server
OUT=/private/tmp/claude-2100076093/-Users-riku-ito-Workspaces-sandbox-private-isu/20ed904c-627b-411e-8c7e-c29224b4488f/scratchpad
T=http://localhost
PPROF=http://localhost:6060
mkdir -p "$OUT"

speedlimit() { pmset -g therm 2>/dev/null | grep -oE 'CPU_Speed_Limit *= *[0-9]+' | grep -oE '[0-9]+$' | tail -1; }
wait_thermal() { for _ in $(seq 1 30); do lim=$(speedlimit); lim=${lim:-100}; echo "CPU_Speed_Limit=$lim"; [ "$lim" -ge 100 ] && return; echo "throttled -> rest 30s"; sleep 30; done; }

echo "=== cooldown 120s + thermal wait ==="; sleep 120; wait_thermal

echo "=== load (bg) + CPU/heap profile ==="
( for i in 1 2; do "$BIN" -u "$UD" -t "$T" >/dev/null 2>&1; done ) &
LOADPID=$!
sleep 8
go tool pprof -seconds=30 -proto -output="$OUT/cpu.pb.gz" "$PPROF/debug/pprof/profile" 2>&1 | tail -2
go tool pprof -proto -output="$OUT/heap.pb.gz" "$PPROF/debug/pprof/heap" 2>&1 | tail -2
wait $LOADPID 2>/dev/null || true

go tool pprof -top -cum -nodecount=35 "$SERVER" "$OUT/cpu.pb.gz" 2>/dev/null > "$OUT/cpu_top_cum.txt"
go tool pprof -top      -nodecount=35 "$SERVER" "$OUT/cpu.pb.gz" 2>/dev/null > "$OUT/cpu_top_flat.txt"
go tool pprof -top -nodecount=25 -sample_index=alloc_space "$SERVER" "$OUT/heap.pb.gz" 2>/dev/null > "$OUT/heap_alloc.txt"
echo "profile reports -> $OUT/{cpu_top_cum,cpu_top_flat,heap_alloc}.txt"

echo "=== rested baseline (cooldown 60s) ==="; sleep 60; wait_thermal
echo "warmup"; "$BIN" -u "$UD" -t "$T" 2>/dev/null
for i in 1 2 3; do echo "BASE run $i:"; "$BIN" -u "$UD" -t "$T"; [ "$i" -lt 3 ] && sleep 20; done
echo "PROFILE_AND_BASELINE_DONE"
