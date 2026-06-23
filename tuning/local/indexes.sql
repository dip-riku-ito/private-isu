-- ローカル再現: チューニング版で適用する索引 (tuning/01_indexes.sql + 02_indexes.sql)
-- compose up 後に docker compose exec で流す。
-- 注: MySQL は ADD INDEX IF NOT EXISTS を非対応。再実行時の "Duplicate key name" は
--     run.sh が mysql --force で無視するため冪等に扱える。
ALTER TABLE comments ADD INDEX idx_post_created (post_id, created_at);
ALTER TABLE posts    ADD INDEX idx_user_id (user_id);
ALTER TABLE posts    ADD INDEX idx_created_at (created_at);
ALTER TABLE comments ADD INDEX idx_user_id (user_id);
