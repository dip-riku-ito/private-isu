-- Step 5: 追加インデックス
-- getAccountName の `SELECT COUNT(*) FROM comments WHERE user_id=?` が
-- comments(user_id) 索引なしで10万行フルスキャンになっていたため追加。
-- サーバ側にも /home/isucon/tuning/02_indexes.sql として保存。

ALTER TABLE comments ADD INDEX idx_user_id (user_id);
