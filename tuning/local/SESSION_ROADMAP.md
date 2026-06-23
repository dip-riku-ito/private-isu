# ローカル改善セッション ロードマップ（自律ループの状態ファイル）

> このファイルはオーケストレータ(Claude)が自律改善サイクルを回すための**永続状態**。
> context要約に耐えるよう、ここを読めば現状と次アクションが分かるように保つ。

## 環境・前提（固定）
- ローカル固定スペック: cpuset "0,1"(2コア共有) + mem3.5GB（EC2 c6i.large模擬）。ベンチはmacOSホスト別コア。
- 対象: `webapp/golang/`(branch=Step19) + `tuning/local/nginx-conf.d/isu.conf` + `tuning/my.cnf`。
- 計測: `tuning/local/measure.sh <label>` = **発熱対策(pmset therm監視)cooldown → warmup1 + 本計測3** の中央値。
  - **発熱でスロットルすると値が下振れ**するため、CPU_Speed_Limit=100 回復まで休息してから計測。
  - 各計測はバックグラウンド実行→完了通知で次サイクルへ自律継続。
- 採否基準: 中央値が**+3%以上**かつ **fail=0** なら採用（<3%はノイズ圏として原則不採用、ただし総work削減で害がなければ維持可）。回帰/fail>0 は即revert。
- レギュレーション安全(必ず維持): fail0 / renderはバイト等価(render_test.go) / account_nameバリデーション緩和禁止 / /postsキャッシュのCSRF跨ぎ(既知HIGH)を悪化させない。

## サイクル手順
1. 施策選定（このファイルの優先度順）
2. 実装（私が直列。必要なら implementer agent をworktreeで）
3. **安全レビュー**（code-reviewer agent が diff を独立レビュー → fail0/バイト等価/セキュリティ）
4. 計測（measure.sh、cooldown込み、bg）
5. 採否＆記録（このファイル + 効果あれば TUNING_LOG + commit）
6. **2施策ごとに再プロファイル→優先度見直し**（このセクションを更新）

## ベースライン
- Step19 初回ローカル計測(冷却なし): median 636,713 (fail0)
- **rested再計測(profile.sh, 発熱対策あり): median 615,880**（566084/615880/623903, fail0, 分散±5%）← **比較基準**

## プロファイル(ローカル実測 2026-06-23・EC2と律速が違うと判明)
- CPU: `scratchpad/cpu_top_cum.txt` / `cpu_top_flat.txt`、heap: `scratchpad/heap_alloc.txt`
- **所見: ローカルはアロケーション律速**。
  - CPU flat: Syscall6 22.7%(応答書込/DB socket) / mallocgc 15.1%cum / memclrNoHeapPointers 8.5% / memmove 3.3%。
  - heap alloc_space: **bytes.growSlice 33.9% + strings.Builder.WriteString 20.9%(=render) + io.ReadAll 15.7%(=画像upload) + reflect系(sqlx scan) ~13%** = 全体の84%。割当 ~800MB/s。
  - **セッション系allocは合計~8-10%**でCPU上位に出ない → EC2(12%CPU)より局所優先度は低い。
  - 結論: **GC頻度低減(GOGC) と render/upload のアロケ削減が最も効く**。応答書込(syscall)はnginxキャッシュ拡大で app をオフロードする方向。

## 施策候補（優先度順・profile加味で確定）
| # | 施策 | 観点 | 期待効果 | リスク | 状態 | 結果 |
|---|---|---|---|---|---|---|
| C1 | **appgo-2** GOGC=200 + GOMEMLIMIT=768MiB | GC | +2〜5%(mallocgc15%直撃) | low(env/即revert) | **選定** | |
| C2 | **appgo-3** render strings.Builder を sync.Pool 化 | alloc | +1〜3%(Builder21%alloc) | low(バイト等価) | 待 | |
| - | db-1 comment_count非正規化+LIMIT3 | DB | +5〜12% | med(schema/init/postComment) | 候補(再profile後) | |
| - | infra-2 /posts/:id microcache(+infra-1 CSRF是正同時) | cache | +5〜15% | med(stale/CSRF) | 候補(再profile後) | |
| - | appgo-6 upload io.ReadAll を事前確保buffer | alloc | +0.5〜1% | low | 候補(C2に同梱可) | |
| - | db-4 timelineカバリング索引(created_at,user_id,mime) | DB | +1〜2% | low | 候補 | |
| - | appgo-1 regexp をpkg var化 | micro | <0.5% | low | 候補(同梱可) | |
| - | infra-3/4 MySQL buffer_instances/io_capacity | DB設定 | +1〜3% | low | 候補 | |
| - | appgo-4 セッション軽量化(HMAC+JSON) | app | EC2は+5-8%だがローカルは~8-10%alloc止まり | med-high(大diff) | 保留(再profile後判断) | |
| - | db-5 comments ORDER BY ASCでfilesort排除 | DB | 条件付(要EXPLAIN先行) | low-med | 条件付 | |
| - | db-2/db-3 comment/userstats memcache | DB | +1〜8% | med | 候補(db-1後) | |
| - | infra-5 proxy_cache_use_stale(雷群/variance改善) | cache | +1〜2%・分散減 | low | 候補 | |

### 判断方針
- まず**低リスク即効のアロケ/GC系(C1→C2)を2サイクル**回す（user指示の「2回やったら見直し」に合致）。
- C1/C2後に**再プロファイル**→アロケ律速がどれだけ解消したか見て、次を db-1(DB) か infra-2(cache offload) に決める。
- appgo-4(セッション)はローカルでは上位でないため、cheapな砲を撃ち切ってから再判断。

## サイクル履歴
- cycle 0: 環境構築・rested baseline=615,880・プロファイル採取・Ideationチーム3体で候補起案・優先度確定。
- **cycle 1: appgo-2(GOGC=200+GOMEMLIMIT=768MiB)** → median **624,151**(572/624/628k, fail0)。baseline比 **+1.3%=ノイズ圏**。
  - 判定: **維持**(env-only/無害/総GC work削減・多コア機ではSTW減で効く想定)。ただしローカルでは効果ほぼ無し。
  - **重要な学び: プロファイルで app CPU=2コア枠の~25%(0.5コア)しか使っておらず app は CPU律速でない**。→ app側micro-opt(GOGC/sync.Pool)は局所天井が低い。律速は ①2コア共有プールをmysql/nginxが食っている or ②syscall/network/DB往復のI/O待ち の可能性。
  - → **C2(sync.Pool)を機械的に続ける前に、docker statsで2コアの配分を実測してロードマップ見直し**(user指示の「2回やったら見直し」を前倒し・データ駆動で)。

### 🔍 ロードマップ見直し #1（cycle1後・docker stats実測）
負荷中の各コンテナCPU(cpuset 2コア=200%が上限):
| サービス | CPU% | メモリ |
|---|---|---|
| nginx | **~54%(最大)** | 76MiB/500 |
| app | ~46% | ~30MiB/1000 |
| mysql | ~23% | **1.508GiB/1.562(98%)** |
| memcached | ~4% | 68MiB/400 |
| 合計 | **~127%/200%** | |

- **結論: 2コアプールは約63%しか使われていない=CPU律速でない**。→ C1(GOGC)がノイズだった理由。律速は**レイテンシ/往復/並行度**。
- nginxが最大消費(gzip+proxy_cache+静的)。mysqlメモリがほぼ上限(buffer_pool=1G)。
- **戦略変更**: app側CPU micro-opt(sync.Pool=C2, session=appgo-4)は**局所天井が低いので後回し**。レイテンシ/オフロード系を先に検証する。
- **次サイクル C2 を sync.Pool → infra-1+infra-2 に差し替え**:
  - infra-1: /posts proxy_cache に Cookie bypass 追加（既知HIGHのCSRF跨ぎを是正・安全修正）。
  - infra-2: /posts/:id に Cookie bypass付き microcache(60s) 追加（匿名の投稿詳細を app+mysql からオフロード→レイテンシ削減）。
  - これが効けば「レイテンシ/オフロード律速」、効かなければ「bench load-gen律速」の判別になる（次の戦略の分岐点）。
  - config-only/即reload・即revert可・低リスク。検証はcurlでX-Cache(MISS→HIT/BYPASS)とCSRFトークン空を確認＋bench fail0。

- **cycle 2: infra-1+infra-2(nginx cache: /posts CSRF是正 + /posts/:id microcache)** → median **500,901**(440/501/505k, fail0)。baseline比 **−18.7%＝明確な回帰** → **revert**。
  - 検証は全パス(匿名HIT/Cookie有BYPASS/csrf_token="")＝実装は正しい。が**スコアは大幅悪化**。
  - **原因(重要)**: ①nginxが既に最大CPU→キャッシュ追加は最ホット側に仕事を乗せ悪化。②**infra-1のCSRF是正でログイン済み/postsがbypass→毎回app makePosts+comments(~2000行)に落ちる**。**CSRF跨ぎ漏洩バグは「ログイン済み/postsを激安にする不正な下駄」だった**。安全版のコスト=**約-18%**(実測)。
  - 判断: スコア最優先で revert(基準を戻す)。**CSRF是正は安全 vs -18%のユーザー判断材料として別途提示**(TUNING_LOGで元々"判断待ち")。

### 🔍 ロードマップ見直し #2（cycle1-2後・user指示の「2回で見直し」）
2サイクルの結論:
- **ローカルはCPU律速でない(プール63%)＋nginxが最ホット**。よって:
  - app側CPU micro-opt(GOGC/sync.Pool/session) → ノイズ(C1で実証)。
  - nginxへのオフロード(キャッシュ追加) → **逆効果**(C2で実証)。
- **C2が-18.7%動いた=server-side変更はスコアに効く(bench load-gen律速ではない)**。律速はレイテンシ/各コンポーネントの仕事量配分。
- **次に効きうるのは「総work削減で、かつnginxに仕事を移さない」もの**:
  - 第一候補: **db-1(comment_count非正規化+LIMIT3)** = 認証GET/・/postsのmakePostsが毎回舐めるcomments ~2000行を~60行に削減。app(46%)とmysql(23%)の両方を**減らす**(どこにも仕事を移さない)。ローカルの最ホット経路を直撃。
  - 低リスク補助: db-4(timelineカバリング索引), db-5(filesort確認), appgo-6(upload buffer)。
  - nginx自体のCPU削減(gzip cost)も候補だが、bytes増とのトレードオフで不確実。
- **次サイクル C3 = db-1**(med risk → 実装は慎重に＋code-reviewerで安全レビュー＋render_test.go＋fail0確認)。

- **cycle 3: db-1(comment_count非正規化 + コメント最新3件LATERAL JOIN)** — **実装完了・計測保留**。
  - 変更: Post struct(db:"comment_count")/dbInitialize再計算/4 SELECTにcomment_count/makPostsをLATERAL分岐(allCommentsは全件IN維持)/postComment +1/indexes.sqlに列追加。
  - 自己精査(code-reviewerはclassifier障害で起動不可のため代替): LATERAL構文・?数・表示順反転・allComments分岐・CommentCount経路 整合OK。
  - **ブロッカー: Claudeのsafety classifier障害でBash/Agent実行不可** → ビルド/索引適用/go test/計測/レビューが保留。
  - 復帰時の手順: `tuning/local/apply_c3.sh`(nginx reload→列ALTER→go build→go test→app再起動→smoke) → 問題なければ `tuning/local/measure.sh c3-db1 120`。
  - **fail>0 か go test 失敗なら db-1 を即revert**。

- **cycle 3 結果: db-1 = 中立（回帰ではない）→ revert維持**。
  - db-1計測(per-run冷却): 544,726 / 529,794 ≈ 537k。
  - **fresh baseline(db-1適用前・今・同条件)再測定: median 537,376**(537/544/537, fail0)。
  - db-1(537k) ≈ fresh baseline(537k) → **db-1は中立**。go testもPASS(バイト等価)。実装はscratchpad/app_db1.go.bakに退避(EC2多コアで再評価する余地。LATERALのper-postオーバヘッドが局所では効かず)。
  - revert済(git checkout app.go = Step19)。comment_count列はDBに残存(無害)。

### 🚨 重大な計測上の発見: マシンの下方ドリフト
- **当初baseline 615,880(session開始時) → fresh baseline 537,376(現在) = 約 −12.7%**。
- per-run冷却で各runを冷やし CPU_Speed_Limit=100 でも低い → ハードスロットルではなく**長時間ベンチの蓄熱でブースト持続クロックが低下**(または電源/背景負荷)。
- **意味: マシンのドリフト(~13%)が施策効果(数%)より大きい。stale baselineとの比較は無効。**
  - 再評価: C1(+1.3%)=実質中立。C3=中立。C2(-18.7%)はCSRF是正で認証/postsがapp直行する**真の回帰**(ドリフトを差し引いても大)。
- **計測法の恒久修正(必須)**: 各サイクルで **baselineブロックと施策ブロックを back-to-back で測り、その場のbaselineと比較**(stale値は使わない)。基準は毎回較正。効果は >~8-10% で初めて有意とみなす。
- **較正後の現基準 = 537,376**(以後の比較はこの「その場baseline」方式で)。

- **cycle 4: nginx gzip off（interleaved A/B・採用）** ✅ 今セッション初の実改善。
  - 計測法: nginx設定はreload(~1s)で差し替え可→ **BASE(gzip on)とTREAT(gzip off)を1runずつ交互×3ペア**でドリフトをペア内相殺。
  | pair | BASE(on) | TREAT(off) | Δ |
  |---|---|---|---|
  | 1 | 494,613 | 525,627 | +6.3% |
  | 2 | 504,696 | 538,825 | +6.8% |
  | 3 | 506,027 | 634,390 | +25.4% |
  | 中央値 | 504,696 | 538,825 | **+6.8%** |
  - 3ペア全て gzip off 有利。**nginxが最ホット(~54%)→gzip圧縮CPU削減が効く**。localhost配信ではバイト増デメリット小。
  - 採用: `nginx-conf.d/isu.conf` を `gzip off` に。**cacheに旧gzipエントリが残るためパージ必須**(rm /var/cache/nginx/*)→反映確認済(Content-Encoding無)。
  - 注: A/B中はcache HITが旧gzipのままだったため +6.8% は控えめ値。実効はそれ以上の可能性。
  - ⚠️ EC2(実ネットワーク)では帯域・転送量が効くため gzip off は再評価必須(localhost特有の結果かも)。

## セッション総括（一時停止 2026-06-24）
- 4サイクル: C1 GOGC=中立 / C2 nginxキャッシュ拡大=回帰(revert) / C3 db-1=中立(revert) / **C4 gzip off=採用 +6.8%**。
- 最大の教訓: **マシンが~13%下方ドリフト(615k→537k)。施策効果(数%)より大きい→stale baseline比較は無効。nginx設定はreloadが速いので「interleaved A/B(BASE/TREAT 1runずつ交互)」が有効でドリフトを相殺できる**(C4で実証)。今後の計測はこの方式を標準にする。
- 局所律速の理解: not CPU律速(プール63%) / not DB律速(mysql23%) / **nginxが最ホット**。よって効いたのはnginx CPU削減(gzip off)のみ。app/DB系のmicro-optは局所では中立。
- 現live状態: Step19 + GOGC(env) + nginx gzip off。fail0。db-1版は scratchpad/app_db1.go.bak に退避。
- 次やるなら: nginx CPUの他の削減(static配信のsendfile確認, proxy_cacheのkey/lock, HTTP/1.1 keepalive見直し)を同じinterleaved A/Bで。app/DB系は多コアEC2でプロファイル根拠評価。

### 🔍 ロードマップ見直し #3（3サイクル+ドリフト発見後）
3サイクルで**局所スコア改善はゼロ**。理由は一貫: **Step19は局所均衡**にある。
- not CPU-bound(63%) / not DB-bound(mysql23%) / nginxが最ホットだがcache追加は逆効果 / app micro-optもGCもDBも非律速。
- 加えて**±5%ノイズ + ~13%ドリフト**で小さい効果は測定不能。
- **戦略判断(ユーザーへ提示)**: ①ローカル最適化は diminishing returns。Step19は良好な到達点。②続けるなら「大きい効果(>15%)が見込めるもの」か「nginx CPU削減/レイテンシ・並行度」など局所の真の律速に絞り、interleaved A/Bで厳密測定。③多コアEC2向けの"総work削減"施策はプロファイル根拠で採否(局所スコアでは測れない)。
