-- ============================================================================
-- 003_citus_rebalance.sql — Phase 2 / ADR-009 Citus 分片重平衡与验证
-- 前置条件：002_citus_sharding.sql 已执行，mail_metadata 与 account_sync_cursor 已声明为按 tenant_id 分布的分布式表。
-- 用途：在数据规模增长后重新平衡 shard，避免热点；提供分布状态验证查询。
-- ============================================================================

-- 守卫：仅当 citus 扩展可用时才执行重平衡（与 002 一致），保证普通 PG 上幂等跳过。
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'citus') THEN
    -- 重平衡 mail_metadata 与 account_sync_cursor 的 shard 分布（按租户体量自动再分配）。
    -- 生产环境建议在低峰期执行；dev/PoC 单 worker 无明显效果但不报错。
    PERFORM rebalance_table_shards('mail_metadata');
    PERFORM rebalance_table_shards('account_sync_cursor');
    RAISE NOTICE 'citus rebalanced: mail_metadata / account_sync_cursor';
  ELSE
    RAISE NOTICE 'citus extension not installed — skipping rebalance (dev/PoC mode)';
  END IF;
END
$$;

-- 验证查询（运维 / SRE 排障用）：
-- 1. 表分布状态：分布键、shard 数量
-- SELECT logicalrelid::text AS table_name,
--        distribution_column, colocationid, shard_count, repartitioncolocationid
--   FROM pg_dist_partition
--   WHERE logicalrelid::regclass::text IN ('mail_metadata', 'account_sync_cursor');
-- 期望：distribution_column = 'tenant_id'，shard_count >= 32（默认）。

-- 2. 单 shard 数据量分布（检查倾斜）
-- SELECT shardid, logicalrelid::text AS table_name,
--        pg_size_pretty(pg_catalog.pg_total_relation_size(logicalrelid::regclass))
--   FROM pg_dist_partition
--   WHERE logicalrelid::regclass::text IN ('mail_metadata', 'account_sync_cursor');
-- 期望：各 shard 大小基本均衡；如严重倾斜，再次执行 rebalance_table_shards 即可。

-- 3. 工作节点状态
-- SELECT nodeid, nodename, nodeport, noderack, isactive, shouldhaveshards
--   FROM pg_dist_node ORDER BY nodeid;
-- 期望：isactive=true，shouldhaveshards=true。

-- 4. 租户级数据分布（验证 colocation）
-- SELECT tenant_id, COUNT(*) AS mail_count
--   FROM mail_metadata
--   GROUP BY tenant_id
--   ORDER BY mail_count DESC LIMIT 20;
-- 期望：与 Citus 拓扑一致，单租户数据落单 shard；查询执行计划无 cross-shard fan-out。
