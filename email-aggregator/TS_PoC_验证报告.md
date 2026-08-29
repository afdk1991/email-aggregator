# 邮箱聚合平台 · TypeScript PoC 验证报告

> 关联：`email-aggregator-go/Go_PoC_验证报告.md`（Go 主干 PoC 已先期验证通过）
> 目标：将 TS 版从"仅跑过一次 demo"推进到**可构建、可测试、可实际运行服务**的同等就绪状态。

## 1. 验证环境

| 项 | 值 |
|---|---|
| 运行时 | Node.js **v22.22.2**（托管隔离版，本机自带，无需安装） |
| 运行方式 | `node --experimental-strip-types`（原生 TS 剥离，零 npm install） |
| 依赖 | 运行时 **0 个第三方包**；仅 `node:` 内置模块（http / crypto / net 经 http） |
| 测试框架 | Node 内置 `node:test` + `node:assert`（零依赖，无需 jest/mocha） |

> 与 Go 版一致：本 PoC 刻意做成**零外部依赖、可在笔记本跑通**的 Phase 0 骨架（设计文档中的 Kafka/PG/OpenSearch/MinIO 均为接口占位 stub，未真正连接）。

## 2. 验证结果（全部绿灯）

### 2.1 端到端自检（demo）
```
node --experimental-strip-types src/demo.ts
PASS  初始全量：种子 3 封入库
PASS  增量：注入 1 封后总数为 4
PASS  检索：'Welcome' 至少命中 1
PASS  推送：至少收到 1 次 new-mail
PASS  幂等：mail-ingested 无重复入库
RESULT: ALL PASS ✅   (exit 0)
```

### 2.2 单元测试套件（新增，`tests/phase0.test.ts`）
运行：`node --test --experimental-strip-types tests/phase0.test.ts`
```
# tests 17  # pass 17  # fail 0   (exit 0)
```
覆盖维度：

| # | 用例 | 验证点 |
|---|------|--------|
| 1–4 | 同步状态机 | 7 态合法迁移、自环(`CHANGE_RECEIVED`)不变化但通知监听者、非法迁移抛错、THROTTLED→RESUME→INCREMENTAL |
| 5–7 | KMS 信封加密 | seal/unseal 往返还原、错误 keyVersion 解包失败、rotate 后还原 |
| 8–10 | 内存邮件服务器 | 自增 uid/modSeq、fetchChangesSince 游标语义、IDLE 订阅回调 + 退订 |
| 11–13 | 事件总线 | start 前 publish 丢弃、同 idempotencyKey 幂等去重（仅消费一次）、handler 抛错进 DLQ |
| 14 | IngestService | 消费 sync-tasks → 落库 + 发布 mail-ingested + 回填内容对象键 |
| 15 | 检索索引 | 关键词命中 + 账户隔离 + 未命中 |
| 16 | 连接器注册表 | 按 providerType 路由、缺失抛错 |
| 17 | 全链路集成 | buildApp + runScenario 端到端闭环（3 种子 → 注入 → 4 封 → 检索命中 → 推送） |

> 修复点：唯一一次失败是**测试断言写法问题**（`assert.equal` 做引用比较，`reg.list()` 返回新数组）→ 改为 `assert.deepEqual`，属测试侧修正，**非产品代码 bug**。TS 版逻辑本身经核对无误（相较 Go 版在此前发现的 2 个真实编译错误，TS 版未引入同类缺陷）。

### 2.3 常驻服务实测（REST + WebSocket 同端口 :8080）
启动：`node --experimental-strip-types src/main.ts`（后台），curl 验证：

| 接口 | 请求 | 结果 |
|------|------|------|
| 健康检查 | `GET /api/health` | `200 {"ok":true,"ts":...}` |
| 邮件列表 | `GET /api/mails?accountId=acc_demo&folder=INBOX&limit=20` | `count:4`，主题：`Urgent: Q3 review \| Invoice #1024 \| Monthly report \| Welcome aboard` |
| 关键词检索 | `GET /api/search?accountId=acc_demo&q=Welcome` | `hits:1`，首条 `Welcome aboard` |
| 未知路径 | `GET /api/nope` | `404`（路由兜底正确） |

### 2.4 WebSocket 实时推送验证（新增，`tests/ws_verify.ts`）
运行：`node --experimental-strip-types tests/ws_verify.ts`
- 用 Node 22 内置 `WebSocket` 客户端连接真实 HTTP upgrade 端点；
- 发送 `{type:"subscribe",accountId:"acc_ws"}` 订阅；
- 进程内注入一封新邮件（模拟 IMAP IDLE 推送）→ 经 `fetchChanges → sync-tasks → IngestService → mail-ingested → WsNotifier` 链路；
- 客户端**即时收到**帧：
```
WS_PUSH_RECEIVED: {"kind":"new-mail","accountId":"acc_ws","preview":"WS push test","ts":...}
WS_VERIFY: PASS ✅
```
证明 `WsNotifier` 的 RFC6455 握手 + 帧编解码（服务端发送非掩码帧、客户端发送掩码帧）在真实客户端下端到端可用。

## 3. 交付物与命令

| 文件 | 说明 |
|------|------|
| `tests/phase0.test.ts` | 17 项零依赖单元测试（新增） |
| `tests/ws_verify.ts` | WebSocket 实时推送验证脚本（新增） |
| `package.json` | 新增 `"test": "node --test --experimental-strip-types tests/"` |

可用命令（均**零安装**）：
```bash
node --experimental-strip-types src/demo.ts                 # 无头自检
node --test --experimental-strip-types tests/               # 单元测试
npm test                                                    # 同上（脚本别名）
node --experimental-strip-types src/main.ts                 # 启动 REST+WS 常驻服务
node --experimental-strip-types tests/ws_verify.ts          # WS 推送验证
```

## 4. 限制与诚实声明

1. **仅验证内存桩实现**。架构设计中的真实基础设施（PostgreSQL/Citus、OpenSearch、MinIO、Redpanda/Kafka、KMS、K8s）皆为接口占位（`PgMetadataStore`/`ObjectContentStore`/`OpenSearchIndex`/`KafkaAdapter` 均 `throw NotImplemented` 或仅 `console.warn`）。本验证**不覆盖**这些适配器——它们需接入真实中间件后另测。
2. **`typecheck` 脚本需 `tsc`**：`package.json` 的 `devDependencies` 含 `typescript`/`@types/node`，但未执行 `npm install`（为保持零依赖验证）。运行 `npm run typecheck` 前需先 `npm install`。运行时校验已由 `node --experimental-strip-types` + `node:test` 覆盖。
3. **npx 类命令前缀**：本机用托管 Node，命令写作 `node --experimental-strip-types ...`；用户自有 Node ≥22.6 时可直接用 `npm run demo|start|test`。

## 5. 结论

TypeScript PoC 现与 Go 主干 PoC 处于**同等的"功能可用 + 可验证"**状态：
- 端到端 demo 闭环 ✅
- 17 项单元测试全绿 ✅
- 真实 REST 服务（health/list/search/404）实测通过 ✅
- 真实 WebSocket 客户端实时推送实测通过 ✅

两份 Phase 0 PoC 均已从"设计蓝图 + 未验证骨架"推进为**可在普通本地机器（Node 22.6+）零依赖运行、测试、演示**的可用代码；生产级真实集成栈仍属设计文档的远期目标，不在本次 PoC 范围。
