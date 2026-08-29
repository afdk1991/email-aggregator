/**
 * 检索与索引层。邮件元数据/正文进 OpenSearch（按 tenantId 路由索引 mail-<tid>，ADR-009），
 * PoC 以 InMemorySearchIndex 实现关键词检索（按 tenant+account 隔离）；OpenSearchIndex 为真实集成 stub。
 *
 * 与 Go 实现对齐：email-aggregator-go/src/search/index.go（tenantId 首参 + Resolve 收敛）。
 */

import type { CanonicalMail } from "../model/canonical.ts";
import { Resolve } from "../tenant/tenant.ts";

export interface SearchHit {
  idempotencyKey: string;
  tenantId?: string;
  accountId: string;
  subject: string;
  from: string;
  preview: string;
  internalDate: number;
}

export interface SearchIndex {
  index(mail: CanonicalMail): Promise<void>;
  /** 关键词检索（subject/from/body 命中），按租户+账户隔离 */
  search(tenantId: string, accountId: string, query: string, limit: number): Promise<SearchHit[]>;
}

export class InMemorySearchIndex implements SearchIndex {
  // `${tenantId}|${idempotencyKey}` -> { mail, text }（防跨租户同键碰撞）
  private docs = new Map<string, { mail: CanonicalMail; text: string }>();

  async index(mail: CanonicalMail): Promise<void> {
    const tid = Resolve(mail.tenantId);
    const text = [
      mail.subject,
      mail.from.map((f) => f.address).join(" "),
      mail.bodyText ?? "",
    ]
      .join(" ")
      .toLowerCase();
    this.docs.set(`${tid}|${mail.idempotencyKey}`, { mail: { ...mail, tenantId: tid }, text });
  }

  async search(tenantId: string, accountId: string, query: string, limit: number): Promise<SearchHit[]> {
    const tid = Resolve(tenantId);
    const q = query.toLowerCase();
    const hits: SearchHit[] = [];
    for (const { mail, text } of this.docs.values()) {
      if (Resolve(mail.tenantId) !== tid) continue;
      if (mail.accountId !== accountId) continue;
      if (!text.includes(q)) continue;
      hits.push({
        idempotencyKey: mail.idempotencyKey,
        tenantId: tid,
        accountId: mail.accountId,
        subject: mail.subject,
        from: mail.from.map((f) => f.address).join(", "),
        preview: (mail.bodyText ?? "").slice(0, 80),
        internalDate: mail.internalDate,
      });
    }
    return hits.sort((a, b) => b.internalDate - a.internalDate).slice(0, limit);
  }
}

/**
 * 真实 OpenSearch 适配器占位。落地时改为（按租户路由索引 mail-<tid>）：
 *   await client.index({ index: `mail-${tid}`, id: idempotencyKey, body: {...} });
 *   await client.search({ index: `mail-${tid}`, q: query });
 */
export class OpenSearchIndex implements SearchIndex {
  private url: string;
  constructor(url: string) {
    this.url = url;
    console.warn(`[OpenSearchIndex] stub: ${url}`);
  }
  async index(_mail: CanonicalMail): Promise<void> {}
  async search(_t: string, _a: string, _q: string, _l: number): Promise<SearchHit[]> {
    return [];
  }
}
