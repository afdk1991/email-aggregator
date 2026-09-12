# Multi-AZ 故障演练手册 (RUNBOOK)

> Phase 3 / §16 部署与运维 / ADR-009 多租户高可用基线

## 1. 演练目标

在单机 Docker 内模拟「双可用区」拓扑，验证邮箱聚合平台在基础设施故障下的恢复能力：

- **RPO**（Recovery Point Objective）：度量流复制延迟造成的数据丢失上限
- **RTO**（Recovery Time Objective）：度量从主节点故障到业务恢复读写的时间
- **跨 AZ 数据一致性**：MinIO site replication 与 Kafka 跨 broker 副本同步

通过定期演练建立「故障 → 切换 → 恢复」的肌肉记忆，避免真实事故时手足无措。

## 2. 拓扑速览

```
┌─────────────── AZ-A (network: az_a) ───────────────┐  ┌─── AZ-B (network: az_b) ───┐
│  pg-primary :5433 (可写)                              │  │  pg-replica :5434 (hot   │
│  kafka-a    :9192 (broker-1)                         │  │  standby, 只读)            │
│  minio-a    :9100/9101                               │  │  kafka-b    :9193         │
└──────────────────────┬───────────────────────────────┘  │  minio-b    :9102/9103    │
                       │                                  └─────────────┬─────────────┘
                       └────────── az_xlink (cross-AZ) ────────────────┘
```

两 AZ 通过 `az_xlink` bridge 网络互通（PoC 模拟；生产为 VPC peering / Transit Gateway）。

## 3. 前置准备

### 3.1 环境变量
在 `deploy/` 下创建 `.env` 或在 shell 中导出：
```sh
PG_USER=agg
PG_PASSWORD=agg-secret
PG_DATABASE=mailagg
MINIO_ROOT_USER=agg
MINIO_ROOT_PASSWORD=agg-secret
```

### 3.2 启动双 AZ 栈
```powershell
cd deploy
docker compose -f docker-compose.multiaz.yml up -d
# 等待 pg-replica 完成 basebackup + 进入 hot standby：
docker compose -f docker-compose.multiaz.yml ps
# pg-replica 状态应为 healthy
```

### 3.3 验证 PG 流复制
```sh
docker compose -f docker-compose.multiaz.yml exec pg-primary \
  psql -U agg -d mailagg -c "SELECT * FROM pg_stat_replication;"
# 期望：state=streaming, sync_state=async, sent_lsn≈replay_lsn
```

### 3.4 验证 Kafka 双 broker
```sh
docker compose -f docker-compose.multiaz.yml exec kafka-a \
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka-a:9092 \
  --describe --topic mail-ingested
# 期望：ReplicationFactor:2, Isr: 1,2
```

### 3.5 初始化 MinIO site replication（首次启动后执行一次）
```powershell
docker compose -f docker-compose.multiaz.yml run --rm mc \
  mc admin replicate add minio-a minio-b
# 期望：Site replication successfully configured
```

## 4. PG 故障演练（RPO/RTO 度量）

### 4.1 自动化执行
```powershell
# 在仓库根目录执行：
.\scripts\drill-chaos.ps1
```
脚本会自动：
1. 创建 `multiaz_drill` 测试表
2. 向 primary 写入 100 行
3. 等待 2s 让流复制追赶
4. 度量 RPO = primary 总行数 - replica 同步行数
5. `docker compose stop pg-primary` 注入故障
6. `pg_promote()` 提升 replica 为新 primary
7. 写入 canary 行验证可写，度量 RTO
8. 输出 PASS/FAIL 报告

### 4.2 PASS 判据
| 指标 | 目标 | 阈值 |
|------|------|------|
| RPO | 0 rows | ≤ 5 rows（允许流复制秒级延迟） |
| RTO | < 30s | ≤ 30s（手动 promote 模式） |

### 4.3 手动分步演练

**步骤 1：在 primary 写入测试数据**
```sh
docker compose -f docker-compose.multiaz.yml exec pg-primary \
  psql -U agg -d mailagg -c "INSERT INTO multiaz_drill (tenant_id) VALUES ('manual-1');"
```

**步骤 2：注入故障**
```powershell
docker compose -f docker-compose.multiaz.yml stop pg-primary
```

**步骤 3：在 replica 上执行 failover**
```sh
docker compose -f docker-compose.multiaz.yml exec pg-replica \
  psql -U agg -d mailagg -c "SELECT pg_promote();"
# 期望：t（promote 成功）
```

**步骤 4：验证 replica 可写**
```sh
docker compose -f docker-compose.multiaz.yml exec pg-replica \
  psql -U agg -d mailagg -c "INSERT INTO multiaz_drill (tenant_id) VALUES ('post-failover');"
```

**步骤 5：业务恢复时间 = 步骤 2 至步骤 4 成功的时间差**

## 5. Kafka broker 故障演练

### 5.1 注入故障
```powershell
docker compose -f docker-compose.multiaz.yml stop kafka-a
```

### 5.2 验证 kafka-b 仍可读写
```sh
docker compose -f docker-compose.multiaz.yml exec kafka-b \
  /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server kafka-b:9092 \
  --topic drill-test <<< "post-failover-message"

docker compose -f docker-compose.multiaz.yml exec kafka-b \
  /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server kafka-b:9092 \
  --topic drill-test --from-beginning --max-messages 1
# 期望：能读到刚才写入的消息（replica 在 kafka-b 上仍可服务）
```

### 5.3 恢复
```powershell
docker compose -f docker-compose.multiaz.yml start kafka-a
# ISR 自动重新同步
```

## 6. MinIO site 故障演练

### 6.1 注入故障
```powershell
docker compose -f docker-compose.multiaz.yml stop minio-a
```

### 6.2 验证 minio-b 仍可读写
```powershell
docker compose -f docker-compose.multiaz.yml exec minio-b mc \
  ls minio-b/agg-mail/  # 列出对象
docker compose -f docker-compose.multiaz.yml exec minio-b mc \
  cp - minio-b/agg-mail/drill-test.txt <<< "post-failover"
```

### 6.3 恢复 + 验证 site replication 追平
```powershell
docker compose -f docker-compose.multiaz.yml start minio-a
# 等待 10s，验证 minio-a 上能看到 step 6.2 写入的对象
docker compose -f docker-compose.multiaz.yml exec minio-a mc \
  ls minio-a/agg-mail/  # 应包含 drill-test.txt
```

## 7. 完整故障切换演练剧本（综合场景）

> 适用于月度演练：模拟整个 AZ 同时挂掉的「最大灾难」场景

### 7.1 场景
- AZ-A 完全失联：pg-primary / kafka-a / minio-a 同时不可达
- 期望：业务流量切到 AZ-B，仍可读 + 部分可写

### 7.2 步骤
```powershell
# 1. 一键停止整个 AZ-A
docker compose -f docker-compose.multiaz.yml stop pg-primary kafka-a minio-a

# 2. PG failover（手动 promote）
docker compose -f docker-compose.multiaz.yml exec pg-replica \
  psql -U agg -d mailagg -c "SELECT pg_promote();"

# 3. Kafka 切换：bootstrap-server 改为 kafka-b:9092
#    应用层只需改 KAFKA_BROKERS 环境变量为 kafka-b:9092 即可

# 4. MinIO 切换：endpoint 改为 minio-b:9000
#    应用层只需改 MINIO_ENDPOINT 环境变量即可

# 5. 业务连接重试，验证可读 + 可写
#    （应用层需有 connection retry + LB 健康检查退避）

# 6. 记录从 AZ-A 停止到业务恢复的时间 = 综合 RTO
```

### 7.3 恢复 AZ-A
- PG：用 `pg_rewind` 让原 primary 反向追赶新 primary，再作为 standby 加入
- Kafka：直接 start，broker 自动 rejoin 集群并追 ISR
- MinIO：直接 start，site replication 自动追平

## 8. 演练频率与责任人

| 演练类型 | 频率 | 责任人 | 期望耗时 |
|---------|------|--------|----------|
| PG 自动化（drill-chaos.ps1） | 每周 | SRE oncall | 5 min |
| Kafka broker 切换 | 月度 | SRE + 后端 lead | 15 min |
| MinIO site 切换 | 季度 | SRE + 数据 lead | 15 min |
| 综合多 AZ 演练 | 半年 | 全员参与 | 60 min |

## 9. 已知限制与生产化差距

| 项 | PoC 模拟 | 生产化 |
|----|---------|--------|
| PG failover | 手动 `pg_promote()` | Patroni / Stolon 自动选主 |
| PG 反向同步 | 不自动 | `pg_rewind` + streaming 重新加入 |
| 应用层重连 | 改环境变量重启 | LB 健康检查 + connection pool 自动 failover |
| DNS 切换 | 无 | Route53 / CoreDNS TTL 60s |
| Kafka ISR 退避 | 无 | `unclean.leader.election.enable=false` + alert |
| MinIO 复制延迟告警 | 无 | Prom scrape `minio_bucket_replication_last_minute_failure_count` |

## 10. 演练后必做

- [ ] 把 RPO/RTO 度量结果记录到 SRE dashboard
- [ ] 故障注入期间应用侧 error rate / latency 数据导出
- [ ] 若 RPO > 5 行：检查流复制参数（`synchronous_commit` / `wal_flush_after`）
- [ ] 若 RTO > 30s：考虑引入 Patroni 自动选主，应用层加 connection pool failover
- [ ] 更新 ADR-004 §7（多 AZ 部署）+ §18（开放问题）

## 11. 联系与升级路径

| 场景 | 升级 |
|------|------|
| RPO 持续 > 10 行 | 升级为同步复制（`synchronous_commit=remote_apply`） |
| RTO 持续 > 60s | 引入 Patroni 自动 failover + LB 健康检查 |
| MinIO site replication 长期 lag | 检查带宽 / 对象数量是否超过单 bucket 上限 |
| Kafka ISR 频繁收缩 | 检查 broker 间网络延迟 / GC 暂停 |

---

**版本**: v1.0 (Phase 3)
**最后更新**: 2026-09-12
**下次演练**: 待排期
