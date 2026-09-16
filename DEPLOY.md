# 邮箱聚合平台 · 线上部署说明（DEPLOY）

> 最后更新：2026-09-15 ｜ 已上线形态：EdgeOne Makers（Serverless）｜ 备选：容器化完整功能版

---

## 一、项目结构与技术栈分析

| 组成 | 技术 | 部署相关性 |
|------|------|-----------|
| 前端 | React 18 + TypeScript + Vite | **纯静态**，构建产物 `index.html` + `assets/`，同源调用 `/api`、`/ws` |
| 后端 | Go 1.26（主干 `email-aggregator-go`） | 默认构建**零第三方依赖**；真实中间件（PG/MinIO/OpenSearch/Kafka）由 `//go:build integration` 标签隔离 |
| 前端/后端对照 | TypeScript PoC（`email-aggregator`） | 仅本地契约对照，不参与线上部署 |
| 数据库 | PostgreSQL（元数据） | ❌ **非必需**——默认形态为 InMemory 内存实现，无库即可运行 |
| 对象存储 / 检索 / MQ | MinIO / OpenSearch / Kafka | ❌ **非必需**——同上，均为内存桩 |
| 定时任务 | 有：`cmd/server` 每 30s 生成演示邮件并推送 | ⚠️ 需**长驻进程**，Serverless 形态下不生效 |
| WebSocket | 有：`/ws?accountId=` 实时推送 | ⚠️ 平台 Node 云函数提供原生 `WebSocketPair`，但当前 Makers 版未接入；容器版已内置 |
| 文件持久化 | 无本地文件写入；Makers 版账户/邮件状态写入 **Blob** | ✅ 适配 Serverless「禁止本地持久化」约束（平台无托管数据库，Blob 即数据库） |

**关键结论**：项目被有意设计成「默认零依赖」，因此**不需要数据库也能完整跑通 REST 与页面**。真正受部署形态影响的只有两件事：**WebSocket 长连接**与**30 秒定时任务**——这两者都需要长驻进程。

---

## 二、免费托管方案推荐与额度限制

### 2.1 方案对比

| 平台 | 能否承载 | 免费额度要点 | 结论 |
|--------|---------|-------------|------|
| **EdgeOne Makers** | 静态站 + Serverless 云函数 | 免费层含全球 CDN、自动 HTTPS、自定义域名；Node 云函数（v20，完整 npm 生态，**支持 WebSocket**）；**无托管数据库**，持久化须用 Blob（免费 1 GB）/KV；单次 wall clock **120 秒**；代码包 128 MB；**禁止本地文件持久化** | ✅ **本次采用**（已连通、可立即交付） |
| Render（Docker） | 长驻容器 | 750 小时/月；**15 分钟无流量休眠**，冷启动 30–60 秒；512 MB 内存；60 分钟构建超时；**无免费持久化磁盘**；免费 PG 已停止新申请 | ✅ 保全功能（WebSocket + 定时任务）的最佳免费路径 |
| Koyeb / Fly.io / Cloud Run | 长驻容器 | 均有免费额度或试用金，需注册并可能绑定支付方式 | 备选 |
| Cloudflare Workers / Vercel | 边缘函数 | 不支持 Go 长驻服务 | ❌ 不适用 |

### 2.2 本次采用方案的能力边界（务必知悉）

已上线的是 **EdgeOne Makers（静态 SPA + Node 云函数 + Blob 持久化）**：

| 能力 | 线上状态 |
|------|---------|
| 页面访问、账户切换、邮件列表/详情 | ✅ 正常 |
| 全文检索（中英文） | ✅ 正常 |
| 标记已读/未读、删除邮件 | ✅ 正常，写入 Blob，**跨实例与重新部署保留** |
| 添加/暂停/删除账户 | ✅ 正常，写入 Blob，**跨实例与重新部署保留** |
| AI 摘要对话 | ✅ 正常（未配置模型端点时降级为本地演示回复，带 `[demo]` 标注，不伪造模型输出） |
| **WebSocket 实时推送** | ⚠️ 平台支持，当前未接入（用 `/api/demo/push` 手动触发代替） |
| **30 秒定时演示推送** | ❌ Serverless 无长驻进程 |
| 数据持久化 | ✅ Blob（strong 一致性），免费额度 1 GB |

**存储键方案**（平台无数据库，Blob 即数据库）：

| Key | 内容 |
|-----|------|
| `accounts/<id>.json` | 单个账户 |
| `mails/<accountId>/<id>.json` | 单封邮件 |

首冷启动若 Blob 为空则写入演示种子；之后一律以 Blob 为准。若 SDK 不可用（依赖未装上、本地跑冒烟），自动降级为进程内存，`/api/health` 的 `mode` 字段会如实显示 `serverless-blob` 或 `serverless-memory`。

需要 WebSocket 或定时任务时，请走 **§七 容器化完整功能版**。

---

## 三、运行时版本与依赖安装

| 项 | 版本/方式 |
|----|----------|
| Node.js | ≥ 22（本地构建前端与跑冒烟测试用；云函数运行时由平台提供） |
| Go | 1.26.6（仅容器版构建需要；Makers 版的 Go 入口已弃用，见 §八排查清单第 1 条） |
| 前端依赖 | `email-aggregator-web` 下 `npm ci`（仅 `react` / `react-dom` 两个运行时依赖） |
| 后端依赖 | `@edgeone/pages-blob@^0.0.14`（唯一依赖，写在 `deploy/edgeone-app/package.json`，**平台自动安装**） |
| CLI | `npm install -g edgeone@latest`（要求 ≥ 1.6.0） |

---

## 四、构建命令与产物路径

```bash
# 1) 前端构建（产物：email-aggregator-web/dist）
cd email-aggregator-web
npm ci
npm run build          # = tsc --noEmit && vite build

# 2) 同步产物到部署工作区（deploy/edgeone-app 即 Makers 项目根）
cp email-aggregator-web/dist/index.html   deploy/edgeone-app/index.html
cp email-aggregator-web/dist/assets/*     deploy/edgeone-app/assets/

# 3) 本地冒烟测试（22 项断言，覆盖全部线上路由）
cd deploy/edgeone-app && node smoke.mjs

# 4) 部署
export PAGES_SOURCE=skills
edgeone makers deploy -n email-aggregator-p003 --json
```

**产物路径**

| 产物 | 路径 |
|------|------|
| 前端页面 | `deploy/edgeone-app/index.html` |
| 前端资源 | `deploy/edgeone-app/assets/` |
| 后端 API | `deploy/edgeone-app/cloud-functions/api/[[default]].js` → 线上 `/api/*` |

---

## 五、环境变量与密钥（严禁硬编码进仓库）

在 `.env.example` 中声明、线上用平台注入。**任何密钥都不得写入仓库**：

```bash
# 平台注入（推荐，不会进仓库）
edgeone makers env set AI_THIRD_PARTY_KEY "sk-xxx"
```

| 变量 | 用途 | 留空行为 |
|------|------|---------|
| `AI_GATEWAY_API_KEY` / `AI_GATEWAY_BASE_URL` | 平台约定的 AI 网关变量 | AI 面板降级为本地演示回复 |
| `AI_SELF_HOSTED_URL/MODEL/KEY` | 自托管/私有化模型（企业租户不出境路径，ADR-010） | 同上 |
| `AI_THIRD_PARTY_URL/MODEL/KEY` | 第三方大模型（公共租户需显式同意 + 脱敏） | 同上 |
| `DEFAULT_TENANT_ID` | 默认租户（ADR-009） | 缺省 `default` |

仓库内已排除密钥：`.gitignore` 忽略 `.env`、`.env.*`、`**/kms-keyring*.json`；CI 通过 `secrets.EDGEONE_PAGES_API_TOKEN` 注入部署令牌。

---

## 六、平台配置文件与一键脚本

| 文件 | 作用 |
|------|------|
| `deploy/edgeone-app/cloud-functions/api/[[default]].js` | Makers 云函数入口，catch-all 承接 `/api/*` |
| `deploy/edgeone-app/.env.example` | 环境变量模板（Makers 会据此在部署时注入同名变量） |
| `deploy/edgeone-app/package.json` | 项目声明（无 build script；`dependencies` 供平台安装 Blob SDK） |
| `deploy/edgeone-app/.gitignore` | 排除密钥、平台产物与 `node_modules` |
| `deploy/edgeone-app/smoke.mjs` | 22 项本地冒烟测试，部署前门禁 |
| `deploy/edgeone-app/deploy.sh` | **一键部署**（bash）：构建 → 同步 → 冒烟 → 部署 |
| `deploy/edgeone-app/deploy.ps1` | **一键部署**（Windows PowerShell） |
| `.github/workflows/deploy-edgeone.yml` | CI/CD：前端或部署区变更时自动冒烟 + 部署 |
| `deploy/container/Dockerfile` | 容器化完整功能版镜像（含 WebSocket + 内嵌 SPA） |
| `render.yaml` | Render Blueprint，免费层一键部署容器版 |
| `.dockerignore` | 裁剪构建上下文，排除缓存与密钥 |

---

## 七、容器化完整功能版（需要 WebSocket / 持久化时）

```bash
# 本地构建并验证（构建上下文为仓库根）
docker build -f deploy/container/Dockerfile -t email-aggregator:latest .
docker run --rm -p 8080:8080 email-aggregator:latest
# 打开 http://localhost:8080

# 部署到 Render 免费层：控制台 → New → Blueprint → 选本仓库（自动读取 render.yaml）
```

该镜像把前端内嵌进 Go 二进制（`-tags webui`），单进程同端口提供 REST + WebSocket + 页面，并保留 30 秒定时推送。

### 7.1 更省事的路径：Render 原生 Go 运行时（推荐，无需 Docker）

`deploy/render-go-native.yaml`：Render 直接识别 `go.mod`，跳过镜像构建，冷启动更快。

```bash
# 部署：Render 控制台 → New → Blueprint → 选本仓库 → 选择本文件 → Apply
# 手工创建时填：
#   Root Directory : email-aggregator-go
#   Build Command  : cd ../email-aggregator-web && npm ci && npm run build
#                    && mkdir -p ../email-aggregator-go/webroot/dist
#                    && cp -r dist/. ../email-aggregator-go/webroot/dist/
#                    && go build -tags webui -o bin/server ./cmd/server
#   Start Command  : ./bin/server
#   Health Check   : /api/health
#   Env            : HTTP_PORT=8080
```

两种 Render 方案二选一即可；原生 Go 版配置更少、构建更快，Docker 版更适合将来要接真实中间件的场景。

---

## 八、验证线上访问的步骤

1. **打开页面**（必须带完整 query，缺 `eo_token` 会 401）：
   在浏览器打开部署给出的完整 URL。
2. **确认接口健康**：浏览器访问 `/api/health`，应返回 `{"ok":true,...,"mode":"serverless-blob"}`（`mode` 为 `serverless-memory` 说明 Blob SDK 未生效，见排查清单第 9 条）。
3. **确认 UI 数据**：页面应显示 **6 个**账户芯片（演示/工作/个人邮箱 + 演示邮箱 3/个人邮箱 1/工作邮箱 1）与对应邮件。
4. **验证检索**：搜索框输入 `Invoice`（英文）或 `复盘`（中文），应即时命中。
5. **验证写操作**：点开一封邮件标记已读、添加/删除一个账户，应立即生效。
6. **验证持久化**（本次修复重点）：添加一个账户后**等待数分钟或重新部署**，该账户应仍然存在——不再随实例回收丢失。
7. **本地门禁**：改动后先跑 `node smoke.mjs`，22/22 通过再部署。

> ⚠️ 用 `curl` 直接请求会返回 302/401（预览网关要求浏览器完成 token 校验），这是预期行为，不是接口故障——请在浏览器中验证。

---

## 九、回滚方式

| 场景 | 操作 |
|------|------|
| 回滚到上一次成功部署 | EdgeOne 控制台 → 项目 → 部署历史 → 选择上一个 Deployment → 回滚/重新部署 |
| 本地回滚代码后重发 | `git revert <commit>` 或 `git checkout <旧版本> -- deploy/edgeone-app`，再 `./deploy.sh` |
| 紧急止血（下线） | 控制台将生产环境切到任意历史版本；或本地 `git checkout` 上一个稳定提交后重新部署 |
| 容器版回滚 | Render 控制台 → 服务 → Deploy → 选择历史镜像版本 Rollback |

**建议**：每次部署前确认 `node smoke.mjs` 全绿，并把 `edgeone-project.json`（项目绑定，
与 `.edgeone/project.json` 必须同为 `makers-1qp5qzh9qml7`）与部署 ID 记录进提交信息，便于定位回滚点。

---

## 十、常见部署失败排查清单

| # | 现象 | 根因 | 处理 |
|---|------|------|------|
| 1 | `Go compilation failed: main.go:51:19 unknown escape` | **平台 Go 构建器在 Windows 下的自身缺陷**（把入口文件内联进生成的 main.go 时引入未转义序列）。已用极简 10 行 handler 复现，与项目代码无关 | 已规避：**改用 Node 云函数**。若必须用 Go，请在 Linux/macOS 或 CI（ubuntu-latest）环境构建 |
| 2 | `package email-aggregator-go/src/api is not in std` | Makers Go 构建器只编译单文件入口，不携带 `go.mod` 与多包子目录 | 入口改为自包含单文件，或改用 Node 云函数 |
| 3 | `/api/*` 返回 HTML 而不是 JSON | 云函数文件缺少语言扩展名（必须是 `.js`/`.go`/`.py`）导致未被识别，请求回退到静态 `index.html` | 确认文件名为 `[[default]].js` |
| 4 | 部署成功但页面空白 / 404 | 前端产物未同步到部署根，或 `assets/` 缺失 | 执行 `deploy.sh` 第 2 步重新同步 `index.html` 与 `assets/` |
| 5 | 访问 URL 返回 401 | 复制 URL 时丢了 `?eo_token=...` 查询串 | 使用**完整** URL（含全部 query 参数） |
| 6 | `curl` 返回 302 | 预览网关需浏览器完成 token 校验 | 改用浏览器访问，非接口故障 |
| 7 | 页面报 CORS / 接口 404 | 前端 `BASE='/api'` 走同源；若把前后端拆到不同域名需改 `src/api/client.ts` | 保持同源部署，或显式配置跨域与 API 基址 |
| 8 | AI 面板一直显示 `[demo]` | 未配置 `AI_THIRD_PARTY_*` 环境变量 | 用 `edgeone makers env set` 注入；留空即为预期的演示降级行为 |
| 9 | 数据仍被重置 / `mode=serverless-memory` | `@edgeone/pages-blob` 未安装成功，云函数降级为内存 | 确认根 `package.json` 的 `dependencies` 含该包且已提交 `package-lock.json`；平台会自动安装。降级时站点仍可用，只是不持久 |
| 9b | 添加账户后刷新又消失 | 旧版为内存态（2026-09-15 前）；或 Blob 写入抛错被静默吞掉 | 升级到 Blob 版；确认 `/api/health` 的 `mode` 为 `serverless-blob` |
| 10 | `Makers project exceeds 40 limit` | 账号项目数达上限 | 控制台删除不用的项目，或用 `-n <已有项目名>` 复用 |
| 11 | Docker 构建超时/过大 | 构建上下文包含了 `node_modules`、`.gopath` 等 | 已提供 `.dockerignore`；确认构建上下文为仓库根 |
| 12 | 无法用 curl/脚本验证线上 | 预览网关对非浏览器请求返回 302/401（含静态资源），属平台访问控制 | 只能在浏览器中验证；或本地跑 `smoke.mjs` 验证逻辑。绑定自定义域名后不受此限制 |
| 13 | 本地 `edgeone makers dev` 的 `/api/*` 返回 301/404 | dev 服务器路由与线上不一致（本地调试工具行为） | 不要以 dev 结果判断线上；以 `smoke.mjs` + 线上浏览器验证为准 |

---

## 附：本次部署结果（2026-09-15）

- 项目 ID：`makers-jnzfzsp1nlt6`
- 部署 ID：`dpm9soclkdcw`（当前线上版本：演示数据清理 + 左侧边栏布局 + logo 回首页 + 删除修复）
  上一版：`dp1uumobmga5`（WS 优雅降级 + AI 文案修正）
- 线上地址：见部署输出（含 `?eo_token=...`，务必整串复制；token 约 3 小时有效）
- 冒烟测试：**23/23 通过**
- 存储：Blob（`mailbox` 命名空间，strong 一致性）；种子账户 3 个
  （`acc_demo` / `acc_work` / `acc_personal`）
- ✅ **已在浏览器实测确认**：`/api/health` 返回 `mode: serverless-blob`、6 账户 / 12 邮件；
  账户切换、列表、模拟收信、中英文搜索、AI 对话、写操作持久化均正常。
  → Blob 持久化生效，不是降级内存。

### 附三：本次改造（演示数据清理 + 布局调整 + logo 跳转）

- 已永久下线 6 个演示账户：`acc_demo3`、`acc_demo32`、`acc_personal1`、`acc_personal11`、
  `acc_work1`、`acc_work11`，其在云函数侧进入 `BANNED_IDS` 黑名单（重新 POST 同名 ID 会被 400 拒绝），
  前端 `RETIRED_ACCOUNT_IDS` 同步过滤，种子数据也不再生成。
- **删除不生效的根因**：Serverless 多实例 + 进程内缓存 → A 实例删除、B 实例仍返回旧数据。
  已改为**单文档存储**（`state.json`，每次请求重新读取 Blob，不做跨请求内存缓存），
  DELETE 返回 `mailsRemoved` 并强制回写，删除后前端立即重新拉取列表。
- 布局：导航菜单改为左侧侧边栏，账户操作/凭据表单下移到侧边栏；「添加账户」按钮移至页面右上角。
- logo：左上角 logo 改为 `<a href="/">`，任意视图点击都会触发 `goHome()` 清空搜索结果与选中态回到首页。

## 附二：已知的平台形态限制

| 能力 | Makers（Serverless） | Render（长驻进程） |
|------|---------------------|-------------------|
| WebSocket 实时推送 | ⚠️ 支持但为 Serverless 形态：Node 运行时原生提供 `WebSocketPair`，可建连但实例会回收，长连接不可靠；当前前端做优雅降级，显示中性的「○ 实时未连接」并指数退避静默探活 | ✅ 原生支持 |
| 30 秒自动演示投递 | ❌ 无长驻进程 | ✅ 保留 |
| 手动「模拟收信」 | ✅ 可代替自动投递 | ✅ |

前端 WS 逻辑已做环境自适应：部署到支持长连接的环境会自动变为「● 实时已连接」，无需改代码。
