-- ============================================================================
-- 002_citus_sharding.sql — Citus 分布式表（生产环境可选）
-- 前置条件：已安装 Citus 扩展并完成 worker 节点注册。
-- 开发/PoC 环境无需执行此文件。
-- ============================================================================

-- 启用 Citus 扩展（需 superuser）。
-- 守卫：仅当 citus 扩展可用时才声明分布式表；纯 postgres:16（开发/PoC 环境）
-- 不含 citus 扩展，整段静默跳过，保证 002 在任意环境均可幂等重入而不报错。
-- 这样 002 既能被 bootstrap.sh 在普通 PG 上安全重放，也能在 Citus 集群上正确分片。
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'citus') THEN
    CREATE EXTENSION IF NOT EXISTS citus;
    -- 将 mail_metadata 声明为分布式表，分布键 = account_id
    -- 同一账户的所有邮件落在同一 shard → 单账户查询无跨节点 fan-out
    PERFORM create_distributed_table('mail_metadata', 'account_id');
    -- account_sync_cursor 同样按 account_id 分布
    PERFORM create_distributed_table('account_sync_cursor', 'account_id');
    RAISE NOTICE 'citus enabled: mail_metadata / account_sync_cursor distributed by account_id';
  ELSE
    RAISE NOTICE 'citus extension not available — skipping distributed table setup (dev/PoC mode)';
  END IF;
END
$$;

-- 验证分布状态
-- SELECT * FROM citus_tables WHERE table_name IN ('mail_metadata', 'account_sync_cursor');

-- ── ADR-009 多租户与 Citus 分布键说明 ───────────────────────────────────────
-- 当前以 account_id 作分布键（同一账户邮件落同一 shard，单账户查询无跨节点 fan-out）。
-- ADR-009 的「逻辑多租户」在共享栈内以 tenant_id 列 + 查询过滤实现隔离，分布键沿用 account_id 即可。
-- 若某租户需「物理隔离」（专属栈 / 数据驻留），可将分布键切换为 tenant_id
-- （create_distributed_table('mail_metadata', 'tenant_id')），使租户级数据共置、跨租户无共享 shard。
-- 这属于「完整隔离（多轮）」路线，本骨架阶段不动分布键，保持单节点 default 租户行为不变。
