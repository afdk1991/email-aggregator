-- ============================================================================
-- 003_read_flag.sql — 为存量库补充 mail_metadata.read 列（幂等，可安全重跑）
-- ----------------------------------------------------------------------------
-- 背景：001_init.sql 早期版本未含 read 列，而 PgMetadataStore.SetRead/UnreadCount
-- 依赖该列；本迁移以 ADD COLUMN IF NOT EXISTS 补齐存量库，避免 42703。
-- 新装库在 001_init.sql 中已内置该列，此文件执行后自然跳过。
-- ============================================================================

ALTER TABLE mail_metadata ADD COLUMN IF NOT EXISTS read BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN mail_metadata.read IS '用户已读状态（前端按已读/未读分组排序；read IS NULL 视为未读）';
