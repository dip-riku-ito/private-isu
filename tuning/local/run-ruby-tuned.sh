#!/bin/bash
# Ruby チューニング版の起動。tuned app.rb適用 → build → 索引 → dumpimages(初期画像FS) → smoke。
set -uo pipefail
ROOT=/Users/riku-ito/Workspaces/sandbox/private-isu
P=isu-ruby-tuned
F=tuning/local/compose.ruby-tuned.yml
cd "$ROOT"

echo "[1] tuned版を webapp/ruby へ overlay(ビルド用・後でgit復元)"
cp tuning/local/ruby-tuned/app.rb            webapp/ruby/app.rb
cp tuning/local/ruby-tuned/unicorn_config.rb webapp/ruby/unicorn_config.rb

echo "[2] up --build (mysql取り込みで初回数分)"
docker compose -p "$P" -f "$F" up -d --build --wait --wait-timeout 900

echo "[2.5] webapp/ruby を git 復元(イメージは焼き込み済=動作に影響なし。リポジトリを汚さない)"
git checkout -- webapp/ruby/app.rb webapp/ruby/unicorn_config.rb

echo "[3] 索引適用(--force冪等)"
docker compose -p "$P" -f "$F" exec -T mysql mysql --force -uroot -proot isuconp < tuning/local/indexes.sql

echo "[4] 初期画像をDB BLOB->FS(dumpimages, 既存はskip)"
( cd "$ROOT/webapp/golang" && \
  ISUCONP_DB_HOST=127.0.0.1 ISUCONP_DB_PORT=3306 ISUCONP_DB_USER=root \
  ISUCONP_DB_PASSWORD=root ISUCONP_DB_NAME=isuconp \
  go run ./cmd/dumpimages )

echo "[5] /initialize + smoke"
curl -s -o /dev/null -w '/initialize=%{http_code} ' http://localhost/initialize
for u in / /login /posts/1 "/@catatsuy" /css/style.css /image/1.jpg; do
  curl -s -o /dev/null -w "$u=%{http_code} " "http://localhost$u"
done
echo
echo "READY (ruby-tuned). 計測: tuning/local/measure.sh ruby-tuned 45"
