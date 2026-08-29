# 邮箱聚合平台 · 前端（email-aggregator-web）

React + TypeScript + Vite 实现的轻量前端，对接 [`../email-aggregator-go`](../email-aggregator-go)（Go 主干版）的
REST/WS 契约，补足架构文档中描述但此前缺失的前端层。

## 技术选型（遵循项目规范）

- **React 18 + TypeScript 5 + Vite 5**：与架构文档「前端 React+TS」一致，构建零配置、热更新快。
- **接口优先**：`src/types.ts` 与 Go 版 `model.CanonicalMail` / `aigateway.ChatRequest` 等契约**一一对齐**（ADR-005 归一化模型）。
- **零违规依赖**：仅引入 `react` / `react-dom`，UI 状态用原生 `useState`，不引入 Redux 等额外框架，契合项目「零外部依赖基线」精神。
- **目录清晰**：`api/`（网络层）、`components/`（展示组件）、`pages/`（页面）、`types.ts`（契约）。

## 对接的后端契约（来自 `email-aggregator-go/src/api/server.go`）

| 端点 | 方法 | 说明 |
|------|------|------|
| `/api/health` | GET | 健康检查 `{ ok: true }` |
| `/api/mails?accountId=&folder=&limit=` | GET | 邮件列表（默认 INBOX，limit 50） |
| `/api/search?accountId=&q=` | GET | 全文检索命中 |
| `/api/ai/chat` | POST | ADR-010 AI 能力路由网关入口（需 integration 构建 + AI 网关挂载） |

> 注：基础 `go run ./cmd/demo.go` 未挂载 AI 网关，故 `/api/ai/chat` 不可用；
> 前端对此做**优雅降级**（面板提示「AI 网关未启用」），不影响邮件列表/检索演示。

## 运行

```bash
# 1) 启动后端（另开终端）
cd ../email-aggregator-go && go run ./cmd/demo.go        # 监听 :8080

# 2) 安装并启动前端
npm install
npm run dev                                              # 开发服务器 :5173，/api 自动代理到 :8080
# 或生产构建
npm run build && npm run preview
```

## 约定

- 所有网络 IO 集中在 `src/api/client.ts`，页面/组件不直接 `fetch`。
- 类型定义以 Go 版 `json` tag 为准，字段命名保持 camelCase（前端）↔ 后端一致。
- 新增后端字段时，先改 `src/types.ts` 再改调用方，保持契约同步。
