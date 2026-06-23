# private-isu 400k-500k 到達ロードマップ（調査結果＋方針）

作成: 2026-06-23 / 現在地: **score 43,763 (fail 0, Step5時点)** / 目標: **400,000〜500,000（約10倍）**

本ドキュメントは Agent Teams（Reg=規定解析 / Bench=実機プロファイル / Verifier=コード監査）の3調査を統合した、根本原因に基づく優先順位付きロードマップ。

---

## 0. 結論（最初に読む）

- **負荷モデルはレイテンシ律速**: ベンチは **60秒固定・11並列ハードコード・ランプアップ無し**。よって **スコア ∝ 1/平均レイテンシ**。現状 ≈ 平均15ms/req、400k には **≈1.6ms/req（約10倍短縮）** が必要。理論上限 ≈660k。
- **飽和 tier は MySQL（CPU）**: 実測 mysqld **129.7%**（≈1.3コア, %wait<1=純CPU） / app 38.9%（うち22%はDB応答待ち） / nginx 8.2% / memcached 0.2%。**app・nginx は余力あり、DBが壁**。
- **根本原因 = タイムライン posts クエリの実行計画**: `getIndex`/`getPosts` の `JOIN users WHERE del_flg=0 ORDER BY created_at DESC LIMIT 20` を、オプティマイザが **idx_created_at で早期終了せず ~1〜2万行を filesort** している（JOINクエリ実測 avg313ms / ~1.1万行）。これが mysqld CPU の実体。
- **最優先（Step6）**: 上記クエリを **STRAIGHT_JOIN + FORCE INDEX(idx_created_at)** で「索引順の早期終了（~21行）」に矯正する。**意味論不変・低リスク・最大効果**。
- ⚠️ **重要な期待値補正**: ベンチが**同一2vCPUホストで~19%消費**しており、SUTの計測上限自体が頭打ち。**ソフト施策だけで本ホスト構成のまま 400k は構造的に困難**。同条件で 400-500k を出す層は **ベンチを別ホスト/増コア**で走らせている可能性が高い。→ 我々の software 施策（Step6-9）は移植可能な実利（本ホストでも 2-4x 余地）。真の 400k は **インフラ同等化（別ホストbench or コア増）**が前提。

---

## 1. レギュレーション（Reg 調査）

### 採点式（`benchmarker/checker/action.go:33-39`）
```
score = Σ(GET成功×1) + Σ(POST成功×3[=1+2]) + Σ(画像投稿成功×6[=5+1])
        − Σ(5xx/4xx/DOM不正×10) − Σ(timeout/通信エラー×20)
```
- **304 Not Modified も成功 +1**（キャッシュ活用で加点）。
- POST=実質+3、画像投稿=実質+6。

### 負荷モデル（`benchmarker/cli.go:24-27,131-138`）
- 実行 **60秒固定**、`-c` 無し、**11並列固定**（loadIndex2/indexMoreAndMore2/userAndPostPage2/login2/comment1/postImage1/ban1）、**ランプアップ無し**。
- ∴ throughput はベンチ並列度ではなく **アプリ/DBレイテンシ**で決まる。**レイテンシ半減 ≈ スコア2倍**。

### 点の源泉
- **GET /** と **GET /image/*** で約70%（loadIndex×2 + indexMoreAndMore×2）。次いで **GET /posts?max_created_at=**。

### キャッシュ安全境界
| 対象 | 可否 | 理由 |
|---|---|---|
| 静的(css/js/favicon)・投稿画像 | ✅ 積極キャッシュ | ベンチがETag/Last-Modified送信、**304で+1点** |
| `GET /posts?max_created_at=` | ✅ キャッシュ可 | タイムスタンプ基準で内容不変 |
| `GET /`（トップ） | ❌ キャッシュ不可 | banScenario が **ban即時反映**を検証 |
| `GET /@user` | ⚠️ 短期のみ注意 | ban後の投稿消去チェックは緩いが要注意 |

---

## 2. 実機プロファイル（Bench 調査・ベンチ1回 score43758/fail0 と並走採取）

### CPU占有（2vCPU=200%上限, pidstat平均%CPU）
| プロセス | %CPU | 備考 |
|---|---|---|
| **mysqld** | **129.7** | usr116/sys13, %wait0.7 = 純CPUバウンド・最大消費 |
| app(Go) | 38.9 | うち %wait22 = MySQL応答待ち |
| nginx | 8.2 | worker2本計 |
| memcached | 0.2 | セッション用途・軽負荷 |
| (benchmarker) | 19.2 | **同一ホストでSUTとCPU競合** |
→ 合計≈196%でほぼ飽和。**壁 = MySQL(CPU)**。

### nginx エンドポイント別 合計時間 Top8（sum_s / count / avg_ms）
| endpoint | sum_s | count | avg_ms |
|---|---|---|---|
| **GET /** | 372.0 | 1050 | 354 |
| **GET /posts** | 108.3 | 280 | 387 |
| GET /posts/* | 75.4 | 2615 | 29 |
| GET /@user | 20.9 | 365 | 57 |
| GET /js/* | 16.0 | 6606 | 2.4 |
| GET /favicon | 9.0 | 3303 | 2.7 |
| POST /login | 8.6 | 765 | 11 |
| POST / | 8.0 | 134 | 59 |
※ `/image/*` はTop12圏外（Step3のFS化が奏功・画像は律速でない）。

### クエリ別 合計時間 Top（performance_schema 累積。slow_log は共有ホスト制約で代替）
| クエリ群 | 合計s | 回数 | avg | スキャン行 |
|---|---|---|---|---|
| posts ORDER BY created_at 系 | 3162 | 34322 | 92ms | **~2万行** |
| posts p (JOIN/絞り込み)系 | 2627 | 8387 | **313ms** | ~1.1万行 |
| `SELECT id,account_name,del_flg FROM users` | 108 | **67811** | 1.6ms | 全ユーザー毎リク |
※累積値（過去のStep4以前フルスキャンも混入）だが、nginx現行avg(354/387ms)と整合 → タイムラインクエリが mysqld CPU の実体。

### MySQL 設定（現状）
- `innodb_buffer_pool_size=128MB`（小/デフォルト）
- `innodb_flush_log_at_trx_commit=1`, `log_bin=ON`, `sync_binlog=1`（書込同期コスト高）
- `max_connections=151`, `innodb_redo_log_capacity=100MB`

### memcached
- 使用中（session用途）。cmd_get+1635/cmd_set+1274、hit率≈100%。ボトルネックでない。

---

## 3. コード監査（Verifier 調査・file:line / 期待効果）

| # | 項目 | 現状 | 改善 | 効果 |
|---|---|---|---|---|
| 1 | セッション | memcache保存(app.go:31,79) | 現状維持 | — |
| 2 | **DSN/接続プール** | pool未設定(MaxIdle=2既定)・interpolateParams無(app.go:910-923) | SetMaxOpenConns/Idle, interpolateParams=true | 大 |
| 3 | makePosts全ユーザー走査 | `SELECT ... FROM users`毎リク(app.go:171) | IN絞り or memcacheキャッシュ(ban時invalidate) | 中 |
| 4 | **静的配信** | css/js/favicon が Go経由(app.go:946) | nginx直配信+expires | 大 |
| 5 | **nginx** | gzip無/keepalive無/open_file_cache無 | gzip, upstream keepalive(HTTP/1.1) | 大 |
| 6 | テンプレート | 毎リク `template.Must(ParseFiles)`(app.go:484,569,625,666…) | 起動時1回パース | 中-大 |
| 7 | count系 | プロフィールで毎回COUNT(app.go:528,535,556) | キャッシュ/集計 | 小-中 |
| 8 | 画像POST | imgdataをDBへ二重書き(app.go:734-755) | FSのみ保存 | 中 |

---

## 4. 優先順位付きロードマップ（期待値＝レイテンシ律速モデル: score∝1/latency）

> 原則: **飽和tier(MySQL)を先に解放**。app/nginx施策はDB解放後に効く（今やっても待ち時間が消えるだけで throughput は伸びにくい）。

| Step | 施策 | 対象tier | 期待効果 | リスク | 状態 |
|---|---|---|---|---|---|
| **6** | **タイムラインクエリ計画修正**: getIndex/getPosts に `STRAIGHT_JOIN + FORCE INDEX(idx_created_at)` で 1〜2万行filesort → ~21行索引早期終了 | MySQL CPU | **大（実績: 43.7k→~132k 約3倍, GET/ 354→43.6ms 8x, EXPLAIN filesort消失, fail0）** | 低（planヒントのみ・意味論不変） | ✅完了 |
| **6.5** | **（運用）ディスク恒久解放**: ログ切詰＋`PURGE BINARY LOGS`＋`disable_log_bin` | infra | **✅ disk 100%→70%・再増殖停止** | 低 | ✅完了(Step7に統合) |
| **7** | **MySQL設定＋nginx静的/gzip/keepalive＋FD上限**（TUNING_LOG Step7+7b） | MySQL/app/nginx | **✅ 132k→156127 (+18%)・mysqld130→44%・壁がapp(Go)へ** | 中（FD上限の見落としで一度38k崩落→是正） | ✅完了 |

| **8** | **DB接続プール＋interpolateParams=true** | app(Go)/MySQL | **✅ 156k→170721 (+9%)・mysqld %wait→0・app(Go)74%継続** | 低 | ✅完了 |

| **9** | **テンプレート起動時1回パース** | app(Go) | **✅ 170.7k→173720 (+1.7%)・app(Go)74→70.7%（効果限定＝パースは主因でない）** | 低 | ✅完了 |
| **10** | **pprofでCPUプロファイル採取**→Go内訳実測→本丸特定 | app(Go) | 診断（次の優先度を正しく決めるため） | 低 | ⏳次 |

### ▶ 進捗サマリ: 43.7k(S5) → 132k(S6) → 156k(S7) → 170.7k(S8) → **173.7k(S9)**。壁は **app(Go) CPU(70.7%)** 継続。app(Go)施策が逓減(+9%→+1.7%)のため、**Step10でpprof実測**しホットパスを特定してから本丸に当てる方針。
| 7 | makePosts全ユーザー走査(67811回)の撲滅: 全ユーザーを memcache/メモリへcache（ban時invalidate）or 出現user_idをIN絞り | MySQL CPU/app | 中-大 | 中（ban整合） | 未 |
| 8 | MySQL設定: buffer_pool 128MB→1GB(動的`SET GLOBAL`可), flush_log_at_trx_commit 1→2, sync_binlog/binlog OFF(要restart) | MySQL | 中 | 低-中（restart要否） | 未 |
| 9 | app/nginx層: DSN `interpolateParams=true`+接続プール, テンプレ起動時1回パース, 静的css/js/faviconをnginx直配信+expires(304=+1点)+gzip+upstream keepalive | app/nginx | 中（DB解放後に効く） | 低 | 未 |
| 10 | `GET /posts?max_created_at=`のキャッシュ(規定上安全), getAccountName複合索引(user_id,created_at), 画像POSTのDB BLOB二重書き廃止 | mixed | 小-中 | 低 | 未 |
| ∞ | **インフラ同等化**: benchmarker を別ホスト/コア増（本ホストはbenchが19%食いSUT上限を抑制） | infra | 大（真の400k前提） | — | 要判断 |

### 期待値の見立て（粗）
- Step6（本命）: mysqld CPU の最大消費を断つ → **2-3x（≈80-150k）狙い**。
- Step7-9 積み上げ: app/接続/静的を解放 → さらに 1.5-2.5x（**≈150-250k 圏**）。
- **400-500k**: 上記 software に加え **bench別ホスト化（実効コア+1）が事実上必須**。同条件の高スコア層はこの構成と推定。

---

## 5. 計測プロトコル（厳守）
- **ウォームアップ1回（破棄）＋本計測2回の中央値**（ユーザー要望でStep8以降3回→2回に短縮）。単発はvariance大。
- 各Step: ローカル編集 → EC2デプロイ(`go build`→`systemctl restart isu-go`) → EXPLAIN/温間計測 → Verifier検証 → `tuning/TUNING_LOG.md`追記 → コミット。
- 秘密情報(pem/credential/settings.local)はコミット除外。
