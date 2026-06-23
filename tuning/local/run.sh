#!/bin/bash
# ローカル再現環境の起動。 usage: tuning/local/run.sh [baseline|tuned]
#   - ホストで server をクロスコンパイル(社内TLS傍受でコンテナ内 go mod download が不可のため)
#   - 固定スペック(cpuset "0,1" 2コア共有 + mem合計3.5GB)で compose 起動
#   - tuned のみ: 索引適用 + 初期画像をDB BLOBからFSへ書き出し(dumpimages)
set -euo pipefail

MODE="${1:-tuned}"
ROOT="/Users/riku-ito/Workspaces/sandbox/private-isu"
cd "$ROOT"

case "$MODE" in
  baseline)
    P=isu-baseline
    F=tuning/local/compose.baseline.yml
    GODIR="/Users/riku-ito/Workspaces/sandbox/private-isu-baseline/webapp/golang" ;;
  tuned)
    P=isu-tuned
    F=tuning/local/compose.tuned.yml
    GODIR="$ROOT/webapp/golang" ;;
  *)
    echo "usage: $0 [baseline|tuned]"; exit 1 ;;
esac

echo "[1/4] cross-compile server (linux/arm64) on host ..."
( cd "$GODIR" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o server . )

echo "[2/4] docker compose up (-p $P) ... (初回はMySQLのdump取り込みで数分)"
docker compose -p "$P" -f "$F" up -d --build --wait --wait-timeout 720

if [ "$MODE" = "tuned" ]; then
  echo "[3/4] apply indexes + dumpimages ..."
  # --force: 既存索引("Duplicate key name")を無視して冪等に。MySQLは ADD INDEX IF NOT EXISTS 非対応。
  docker compose -p "$P" -f "$F" exec -T mysql mysql --force -uroot -proot isuconp < tuning/local/indexes.sql
  ( cd "$ROOT/webapp/golang" && \
    ISUCONP_DB_HOST=127.0.0.1 ISUCONP_DB_PORT=3306 ISUCONP_DB_USER=root \
    ISUCONP_DB_PASSWORD=root ISUCONP_DB_NAME=isuconp \
    go run ./cmd/dumpimages )
else
  echo "[3/4] (baseline: 索引/画像FS化はスキップ)"
fi

echo "[4/4] wait for app via nginx (http://localhost/) ..."
code=000
for _ in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w '%{http_code}' http://localhost/ || true)
  [ "$code" = "200" ] && break
  sleep 2
done
echo "GET / -> $code"
[ "$code" = "200" ] && echo "READY ($MODE). ベンチ: tuning/local/bench.sh" || { echo "NOT READY"; exit 1; }
