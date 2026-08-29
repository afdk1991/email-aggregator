-- 005_account_server_host.sql：账户注册表补连接端点列（真实邮箱同步用）。
-- IMAP/POP3/Graph 等连接器在同步时据 server_host 解析真实服务器地址与端口。
-- 幂等：ADD COLUMN IF NOT EXISTS，可安全重跑。
ALTER TABLE mail_account ADD COLUMN IF NOT EXISTS server_host TEXT NOT NULL DEFAULT '';
COMMENT ON COLUMN mail_account.server_host IS '连接端点（如 imap.139.com:993 / imap.gmail.com:993），同步时供连接器解析';
