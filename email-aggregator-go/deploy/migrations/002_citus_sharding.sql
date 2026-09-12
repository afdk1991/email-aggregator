-- ============================================================================
-- 002_citus_sharding.sql — Citus 分布式表（Phase 2 / ADR-009：分布键 = tenant_id）
-- 前置条件：已安装 Citus 扩展并完成 worker 节点注册（deploy/docker-compose.yml 用 citusdata/citus 镜像）。
-- 开发/PoC 环境无需执行此文件（citus 扩展不可用时整段静默跳过）。
-- ============================================================================

-- 启用 Citus 扩展（需 superuser）。
-- 守卫：仅当 citus 扩展可用时才声明分布式表；纯 postgres:16（开发/PoC 环境）
-- 不含 citus 扩展，整段静默跳过，保证 002 在任意环境均可幂等重入而不报错。
-- 这样 002 既能被 bootstrap.sh 在普通 PG 上安全重放，也能在 Citus 集群上正确分片。
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'citus') THEN
    CREATE EXTENSION IF NOT EXISTS citus;
    -- Phase 2 / ADR-009：将 mail_metadata 声明为分布式表，分布键 = tenant_id
    -- 同一租户的所有邮件 + 所有账户落在同一 shard → 租户级数据共置、跨租户无共享 shard；
    -- 满足 ADR-009 「审计数据按 tenantID 分区」硬约束；查询必须带 WHERE tenant_id = ?
    PERFORM create_distributed_table('mail_metadata', 'tenant_id');
    -- account_sync_cursor 同样按 tenant_id 分布（与 mail_metadata colocation 对齐）
    PERFORM create_distributed_table('account_sync_cursor', 'tenant_id');
    RAISE NOTICE 'citus enabled: mail_metadata / account_sync_cursor distributed by tenant_id (Phase 2 / ADR-009)';
  ELSE
    RAISE NOTICE 'citus extension not available — skipping distributed table setup (dev/PoC mode)';
  END IF;
END
$$;

-- 验证分布状态
-- SELECT * FROM citus_tables WHERE table_name IN ('mail_metadata', 'account_sync_cursor');
-- 期望：distribution_column = 'tenant_id'

-- ── ADR-009 多租户与 Citus 分布键说明（Phase 2 已落地）─────────────────────────
-- Phase 2（2026-09-11 立项）正式将分布键从 account_id 切换为 tenant_id：
--   1. 满足 ADR-009 「审计数据按 tenantID 分区」硬约束；
--   2. 同租户数据共置 → 跨账户聚合查询（如租户级统计）单节点扫描，无 fan-out；
--   3. 跨租户查询通过 app 层强制 WHERE tenant_id = ? 路由到对应 shard，避免跨节点；
--   4. 大租户 + 大账户热点风险由 Citus shard rebalance 缓解（见 003_citus_rebalance.sql）。
-- 原 account_id 分布键路线已废弃；如需回退到逻辑多租户共享栈，可在普通 PG 上不安装 citus 扩展即可。
