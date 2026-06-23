#!/bin/bash
# C3(db-1)の適用＋検証(計測の手前まで)。classifier復帰後に一発で流す用。
# 成功したら tuning/local/measure.sh c3-db1 120 で計測。go test失敗やsmokeで非200ならrevert。
set -uo pipefail
ROOT=/Users/riku-ito/Workspaces/sandbox/private-isu
P=isu-tuned
F=tuning/local/compose.tuned.yml
cd "$ROOT"

echo "[1] nginx reload (C2 revert適用) ..."
docker compose -p "$P" -f "$F" exec -T nginx nginx -t && docker compose -p "$P" -f "$F" exec -T nginx nginx -s reload

echo "[2] ALTER: posts.comment_count 追加(+索引, --forceで冪等) ..."
docker compose -p "$P" -f "$F" exec -T mysql mysql --force -uroot -proot isuconp < tuning/local/indexes.sql
docker compose -p "$P" -f "$F" exec -T mysql mysql -uroot -proot isuconp -N -e \
  "SELECT COLUMN_NAME FROM information_schema.columns WHERE table_schema='isuconp' AND table_name='posts' AND column_name='comment_count'"

echo "[3] go build (linux/arm64) ..."
( cd "$ROOT/webapp/golang" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o server . && echo "BUILD_OK" )

echo "[4] go test (render byte-equiv 等) ..."
( cd "$ROOT/webapp/golang" && go test ./... 2>&1 | tail -20 )

echo "[5] app イメージ再ビルド+再起動 (--build必須: dockerfile_inlineがbuild時にserverをCOPYするため) ..."
docker compose -p "$P" -f "$F" up -d --build app
sleep 3

echo "[6] /initialize でcomment_count再計算 + smoke ..."
curl -s -o /dev/null -w '/initialize=%{http_code}\n' -X GET http://localhost/initialize
curl -s -o /dev/null -w 'GET / =%{http_code}\n'         http://localhost/
curl -s -o /dev/null -w 'GET /posts/1 =%{http_code}\n'  http://localhost/posts/1
curl -s -o /dev/null -w 'GET /@catatsuy =%{http_code}\n' http://localhost/@catatsuy
U='http://localhost/posts?max_created_at=2016-08-01T00%3A00%3A00%2B09%3A00'
curl -s -o /dev/null -w 'GET /posts?max =%{http_code}\n' "$U"
echo "smoke done. 全200なら measure.sh c3-db1 120 へ。"
