-- ============================================================================
-- 006_citus_workers.sql — Phase 3 真实 Citus 集群：worker 节点注册
--
-- 前置条件：
--   1. deploy/docker-compose.citus.yml 已启动（cn + w1 + w2 三个容器均健康）
--   2. 在 cn 容器内执行本脚本：
--        docker compose -f docker-compose.citus.yml exec cn \
--          psql -U agg -d mailagg -f /migrations/006_citus_workers.sql
--
-- 幂等性：
--   - citus_add_node 已存在时返回 notice，不报错；
--   - 重复执行不会重复注册；
--   - 在普通 PG 环境（citus 扩展不可用）整段静默跳过，保证重入安全。
--
-- 注册完成后，配合 002_citus_sharding.sql：
--   002 把 mail_metadata/account_sync_cursor 声明为 distributed by tenant_id，
--   Citus 自动按 shard_count（默认 32）在已注册 worker 上均匀分布。
-- ============================================================================

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'citus') THEN
    CREATE EXTENSION IF NOT EXISTS citus;

    -- 注册 worker 1（容器 service name = w1）
    -- 签名：citus_add_node(nodename, nodeport, nodegroup DEFAULT 0, noderole DEFAULT 'primary', nodecluster DEFAULT 'default')
    -- group=0 表示自动分配新 group（每个 worker 独立 group = 独立 shard 放置组）
    PERFORM citus_add_node('w1', 5432);
    RAISE NOTICE 'citus worker registered: w1:5432';

    -- 注册 worker 2（容器 service name = w2）
    PERFORM citus_add_node('w2', 5432);
    RAISE NOTICE 'citus worker registered: w2:5432';

    -- 验证：列出所有已注册节点（coordinator 也会列出自身，node_type='coordinator'）
    RAISE NOTICE '--- active nodes ---';
    -- 在 psql 终端可执行：SELECT nodeid, nodename, nodeport, noderole, groupid
    --                        FROM pg_dist_node ORDER BY nodeid;
  ELSE
    RAISE NOTICE 'citus extension not available — skipping worker registration (dev/PoC mode, single-node postgres)';
  END IF;
END
$$;

-- 验证节点拓扑（cn 期望返回 3 行：1 coordinator + 2 worker）
-- SELECT nodeid, nodename, nodeport, noderole, groupid, isactive
--   FROM pg_dist_node ORDER BY nodeid;
