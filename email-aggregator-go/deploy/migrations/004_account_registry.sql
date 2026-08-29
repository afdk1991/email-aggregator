-- ============================================================================
-- 004_account_registry.sql — 连接/账户服务：账户注册表（幂等，可安全重跑）
-- ----------------------------------------------------------------------------
-- 背景：账户注册表承载账户身份 + 连接配置引用 + 生命周期状态（active/paused/error）。
-- 凭据不落明文——仅保存 credentials_ref（指向 Token Vault/KMS 信封加密引用，ADR-004）。
-- 说明：邮件元数据表 mail_metadata 仍以 account_id 分片；本表为账户主数据。
-- ============================================================================

CREATE TABLE IF NOT EXISTS mail_account (
    id              TEXT        PRIMARY KEY,             -- 账户 ID（与 mail_metadata.account_id 对应）
    tenant_id       TEXT        NOT NULL DEFAULT 'default',
    provider        TEXT        NOT NULL DEFAULT 'imap', -- imap / pop3 / ews / gmail
    email           TEXT,
    display_name    TEXT,
    status          TEXT        NOT NULL DEFAULT 'active', -- active | paused | error
    sync_folder     TEXT        NOT NULL DEFAULT 'INBOX',
    credentials_ref TEXT,                                 -- Token Vault/KMS 引用，非明文
    last_sync_at    BIGINT      NOT NULL DEFAULT 0,       -- 最近同步时间（Unix 毫秒）
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_mail_account_tenant ON mail_account (tenant_id);

COMMENT ON TABLE  mail_account             IS '账户注册表（连接/账户服务；ADR-004 凭据仅存 KMS 引用；ADR-009 租户隔离）';
COMMENT ON COLUMN mail_account.credentials_ref IS 'Token Vault/KMS 信封加密引用，绝不存储明文凭据';
COMMENT ON COLUMN mail_account.status       IS '账户生命周期：active 正常同步 / paused 用户暂停 / error 连接失败待处理';
