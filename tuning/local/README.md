# ローカル再現環境 (EC2消失後の代替)

EC2 が無くなったため、private-isu のチューニング検証を **ローカル(Mac/Docker)** で行うための環境。
`baseline`(チューニング前 = master) と `tuned`(isucon-tuning ブランチ = Step19) を、
**同一の固定スペック**で立ち上げて比較できる。

## ⚠️ 重要: 絶対スコアは EC2 と一致しない

- EC2 は `c6i.large`(2 vCPU / 3.7GiB, amd64) でベンチ同居。ローカルは Mac(arm64) + Docker Desktop(Linux VM)。
- CPU素性・仮想化オーバーヘッド・ベンチ別コアなどが違うため、**TUNING_LOG の数値(283k 等)はローカルでは再現しない**。
- 用途は「**同一マシン上での施策の相対比較**」。baseline → tuned の伸び率や、施策ごとの増減を見るのに使う。

## 固定スペック (EC2 c6i.large 模擬)

競技スタック(nginx/app/mysql/memcached)を **合計 2 vCPU / 3.5GB** に固定:

| 項目 | 値 |
|---|---|
| CPU | 全サービス `cpuset: "0,1"` → 2コアを共有(カーネルが自由にスケジュール = EC2の2 vCPU共有を模擬) |
| メモリ | mysql 1.6G / app 1.0G / nginx 0.5G / memcached 0.4G = 計 3.5G |
| ベンチ | macOS ホスト側で実行(VM外・別コア)。EC2のベンチ同居より競技側はクリーン |

## 前提と既知のハマりどころ

1. **社内TLS傍受**: コンテナ内 `go mod download` が `x509: certificate signed by unknown authority` で失敗する。
   → **ホストで `server` をクロスコンパイル**(`GOOS=linux GOARCH=arm64 CGO_ENABLED=0`)し、実行イメージにバイナリだけ載せる(`dockerfile_inline`)。`run.sh` が自動でやる。
2. **baseline は openssl バイナリ依存**: master のパスワードハッシュは `openssl dgst -sha512` にシェルアウト(Step2でGo内製化する前)。baseline イメージにのみ openssl を同梱。
3. **MySQL の dump 取り込みは数分**: 初回 up は 1.26GB の `dump.sql.bz2`(画像BLOB込み) を取り込むため時間がかかる。healthcheck は `127.0.0.1`(TCP) 強制 = 本サーバ起動後にのみ healthy(初期化中のソケット一時サーバで誤検知しないため)。
4. **tuned は画像をFS配信**: 初期画像(DB BLOB)を `cmd/dumpimages` で `webapp/public/image/` に書き出す。`run.sh tuned` が自動実行。

## 使い方

```sh
# 起動 (どちらか。port 80/3306 を使うので同時起動は不可。切替時は down してから)
tuning/local/run.sh baseline   # チューニング前
tuning/local/run.sh tuned      # チューニング後(索引適用 + dumpimages まで自動)

# ベンチ (ウォームアップ1回 + 本計測3回、中央値を採用)
tuning/local/bench.sh

# 停止
docker compose -p isu-baseline -f tuning/local/compose.baseline.yml down
docker compose -p isu-tuned    -f tuning/local/compose.tuned.yml    down
# DBを作り直す場合は -v でボリュームも削除
```

## ファイル

| ファイル | 役割 |
|---|---|
| `compose.baseline.yml` | baseline スタック(master Go / 素nginx / 素MySQL / BLOB画像) |
| `compose.tuned.yml` | tuned スタック(branch Go / tuned nginx / MySQL設定 / FS画像) |
| `nginx-conf.d/isu.conf` | tuned の nginx(Step3,7,13,15 を Docker向けに調整) |
| `indexes.sql` | tuned の索引(Step1+5、`IF NOT EXISTS`で冪等) |
| `run.sh` / `bench.sh` | 起動 / 計測 ヘルパ |

## 計測結果 (このマシンでのローカル値)

計測プロトコル: ウォームアップ1回(破棄) + 本計測3回の中央値。

| 構成 | スコア(中央値) | runs | fail |
|---|---|---|---|
| baseline (チューニング前) | **9,450** | 9450 / 9502 / 9375 | 0 |
| tuned (Step19) | **636,713** | 603058 / 657569 / 636713 | 0 |

→ 同一固定スペック上で **約 67倍**(9,450 → 636,713)。

> tuned のローカル絶対値(636k)が EC2 実機(283k)より高いのは想定どおり:
> ローカルはベンチを別コア(macOSホスト)で走らせるため競技側2コアがベンチと競合せず、
> かつ Apple Silicon が速いため。**EC2の絶対値とは比較せず、ローカル内の相対比較に使うこと。**

> 注: baseline 用に master を `../private-isu-baseline`(git worktree) に展開している。
> 不要になったら `git worktree remove ../private-isu-baseline`。
