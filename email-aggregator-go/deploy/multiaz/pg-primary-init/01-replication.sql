-- pg-primary-init/01-replication.sql
-- Phase 3 Multi-AZ：在 primary 首次启动时创建复制账号 + 授权
-- 由 postgres:16 镜像自动执行（/docker-entrypoint-initdb.d/）

-- 复制账号：pg-replica 用此身份连接 primary 拉取 WAL
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'replica') THEN
    CREATE ROLE replica WITH REPLICATION LOGIN PASSWORD 'replica-secret';
    RAISE NOTICE 'created replication role: replica';
  END IF;
END
$$;

-- 允许 replica 节点从 0.0.0.0/0 连接（PoC 单机模拟；生产请改为具体 AZ 子网）
GRANT CONNECT ON DATABASE mailagg TO replica;
