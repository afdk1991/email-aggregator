/**
 * BFF / API 网关（PoC）：REST（邮件列表 / 检索 / 健康） + WebSocket 实时推送同端口。
 * 真实环境前置 L7 网关 + 鉴权 + 限流；本文件聚焦接口契约与事件打通。
 */

import { createServer, type Server, type IncomingMessage, type ServerResponse } from "node:http";
import type { MetadataStore } from "../store/metadata.ts";
import type { SearchIndex } from "../search/index.ts";
import { WsNotifier } from "../notify/ws.ts";
import { fromRequest, Resolve, type TenantContext, DefaultTenantID } from "../tenant/tenant.ts";

export interface ApiDeps {
  metadata: MetadataStore;
  search: SearchIndex;
  ws: WsNotifier;
  port: number;
  /** 默认租户上下文（用于无 X-Tenant-Id 头时回退，通常为 DefaultTenantID） */
  tenant?: TenantContext;
}

function json(res: ServerResponse, status: number, body: unknown): void {
  const data = JSON.stringify(body);
  res.writeHead(status, { "content-type": "application/json; charset=utf-8" });
  res.end(data);
}

export class ApiServer {
  private server: Server;

  private deps: ApiDeps;
  constructor(deps: ApiDeps) {
    this.deps = deps;
    this.server = createServer((req, res) => this.router(req, res));
    this.deps.ws.attach(this.server);
  }

  private async router(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? "/", `http://localhost:${this.deps.port}`);
    const q = url.searchParams;

    // ADR-009：从请求头解析租户上下文；缺失回退 DefaultTenantID（或注入的默认 tenant）
    const tctx: TenantContext = (() => {
      const parsed = fromRequest(req);
      // 若请求未携带 X-Tenant-Id，回退到 ApiDeps 注入的默认租户
      const fallback = this.deps.tenant ?? { tenantId: DefaultTenantID, tier: "" };
      const id = parsed.tenantId === DefaultTenantID && fallback.tenantId !== DefaultTenantID
        ? fallback.tenantId
        : parsed.tenantId;
      return { tenantId: id, tier: parsed.tier || fallback.tier };
    })();
    const tid = Resolve(tctx.tenantId);

    if (url.pathname === "/api/health") {
      return json(res, 200, { ok: true, ts: Date.now(), tenantId: tid });
    }

    if (url.pathname === "/api/mails" && req.method === "GET") {
      const accountId = q.get("accountId") ?? "";
      const folder = (q.get("folder") ?? "INBOX") as never;
      const limit = Number(q.get("limit") ?? "50");
      const mails = await this.deps.metadata.listMails(tid, accountId, folder, limit);
      return json(res, 200, { tenantId: tid, accountId, count: mails.length, mails });
    }

    if (url.pathname === "/api/search" && req.method === "GET") {
      const accountId = q.get("accountId") ?? "";
      const query = q.get("q") ?? "";
      const hits = await this.deps.search.search(tid, accountId, query, 50);
      return json(res, 200, { tenantId: tid, accountId, query, hits });
    }

    return json(res, 404, { error: "not found", path: url.pathname });
  }

  listen(): Promise<void> {
    return new Promise((resolve) => this.server.listen(this.deps.port, () => resolve()));
  }
  close(): void {
    this.server.close();
  }
}
