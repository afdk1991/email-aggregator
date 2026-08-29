-- ============================================================================
-- 001_init.sql — 邮箱聚合平台初始 schema
-- 目标数据库：PostgreSQL 16+
-- 执行方式：psql -f 001_init.sql（或通过 golang-migrate / goose 自动迁移）
-- ============================================================================

-- ── mail_metadata: 邮件元数据表 ──────────────────────────────────────────────
-- 每封邮件一行，按 account_id 分区（生产环境用 Citus 分布式表）。
-- id 为协议级稳定标识（如 IMAP UID 组合），作为主键 + 幂等去重键。
-- tenant_id（ADR-009）：逻辑多租户隔离键，与 account_id 共同构成数据归属边界。
CREATE TABLE IF NOT EXISTS mail_metadata (
    id              TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL DEFAULT 'default',
    account_id      TEXT        NOT NULL,
    provider        TEXT,
    folder          TEXT,
    subject         TEXT,
    from_addr       TEXT,
    body_text       TEXT,
    internal_date   BIGINT,
    size_bytes      BIGINT      DEFAULT 0,
    raw_object_key  TEXT,
    cursor_json     JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 按账户 + 文件夹查询（列表页高频路径）
CREATE INDEX IF NOT EXISTS idx_mail_account_folder
    ON mail_metadata (account_id, folder);

-- 按账户 + 时间倒序（收件箱默认排序）
CREATE INDEX IF NOT EXISTS idx_mail_account_date
    ON mail_metadata (account_id, internal_date DESC);

-- 按租户过滤（多租户列表/隔离查询）
CREATE INDEX IF NOT EXISTS idx_mail_tenant_account
    ON mail_metadata (tenant_id, account_id);

-- ── account_sync_cursor: 同步游标表 ─────────────────────────────────────────
-- 每个账户的每个文件夹维护一条游标，记录上次同步的断点。
CREATE TABLE IF NOT EXISTS account_sync_cursor (
    tenant_id       TEXT        NOT NULL DEFAULT 'default',
    account_id      TEXT        NOT NULL,
    folder          TEXT        NOT NULL,
    cursor_json     JSONB,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, account_id, folder)
);

-- ── 表注释（供 psql \d+ 查看） ──────────────────────────────────────────────
COMMENT ON TABLE  mail_metadata        IS '邮件元数据（ADR-003 存储分离：元数据在 PG，正文在对象存储；ADR-009 多租户 tenant_id 隔离）';
COMMENT ON COLUMN mail_metadata.tenant_id IS 'ADR-009 逻辑多租户隔离键（默认 default；企业/私有租户为独立值）';
COMMENT ON COLUMN mail_metadata.id     IS '协议级稳定标识（IMAP: accountId:folder:uid；Exchange: itemId）';
COMMENT ON COLUMN mail_metadata.raw_object_key IS '对象存储引用键（mail/<sha256>，指向 MinIO/S3 中的邮件原文）';
COMMENT ON COLUMN mail_metadata.cursor_json    IS '采集时的游标快照（UIDValidity/UIDNext/ModSeq 等，协议相关）';

COMMENT ON TABLE  account_sync_cursor       IS '同步断点续传游标（ADR-005 多协议归一化；ADR-009 多租户）';
COMMENT ON COLUMN account_sync_cursor.tenant_id IS 'ADR-009 逻辑多租户隔离键';
COMMENT ON COLUMN account_sync_cursor.cursor_json IS 'JSON 序列化的 SyncCursor（uidValidity/uidNext/lastUid/modseq/highWaterMark）';
