-- Step 1: インデックス追加 (score 0 -> 16028, fail 0)
-- comments(post_id) 索引なしによる10万行フルスキャンN+1を解消するのが主目的。
-- MySQLデータディレクトリに永続化されるため再起動耐性あり。
-- サーバ側にも /home/isucon/tuning/01_indexes.sql として保存済み。

ALTER TABLE comments ADD INDEX idx_post_created (post_id, created_at);
ALTER TABLE posts ADD INDEX idx_user_id (user_id);
ALTER TABLE posts ADD INDEX idx_created_at (created_at);
