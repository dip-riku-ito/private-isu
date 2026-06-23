// 既存の posts.imgdata (DB BLOB) を public/image/{id}.{ext} へ一括書き出す一回限りのツール。
// 実行: cd webapp/golang && source /home/isucon/env.sh && go run ./cmd/dumpimages
package main

import (
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	host := env("ISUCONP_DB_HOST", "localhost")
	port := env("ISUCONP_DB_PORT", "3306")
	user := env("ISUCONP_DB_USER", "root")
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := env("ISUCONP_DB_NAME", "isuconp")
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		user, password, host, port, dbname)

	db := sqlx.MustConnect("mysql", dsn)
	defer db.Close()

	dir := "../public/image"
	if err := os.MkdirAll(dir, 0755); err != nil {
		panic(err)
	}

	extByMime := map[string]string{
		"image/jpeg": "jpg",
		"image/png":  "png",
		"image/gif":  "gif",
	}

	rows, err := db.Queryx("SELECT id, mime, imgdata FROM posts")
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	var r struct {
		ID      int    `db:"id"`
		Mime    string `db:"mime"`
		Imgdata []byte `db:"imgdata"`
	}
	dumped, skipped := 0, 0
	start := time.Now()
	for rows.Next() {
		if err := rows.StructScan(&r); err != nil {
			panic(err)
		}
		ext := extByMime[r.Mime]
		if ext == "" {
			continue
		}
		path := fmt.Sprintf("%s/%d.%s", dir, r.ID, ext)
		if fi, err := os.Stat(path); err == nil && fi.Size() == int64(len(r.Imgdata)) {
			skipped++
			continue
		}
		if err := os.WriteFile(path, r.Imgdata, 0644); err != nil {
			panic(err)
		}
		dumped++
	}
	fmt.Printf("dumped=%d skipped=%d elapsed=%s\n", dumped, skipped, time.Since(start))
}
