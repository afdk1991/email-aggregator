/**
 * 检索与索引层。邮件元数据/正文进 OpenSearch（按 tenantId 路由索引 mail-<tid>，ADR-009），
 * PoC 以 InMemorySearchIndex 实现关键词检索（按 tenant+account 隔离）；真实 OpenSearch 适配器
 * 落在 src/integration/integration.ts 的 OpenSearchIndex（fetch-based REST，与 Go 对齐）。
 *
 * Phase 2 / ADR-009（2026-09-11 立项，与 Go 完全对齐）：
 *   - 索引名：mail-<tenantId>（一租户一物理索引；账户在同一索引内以 accountId filter 过滤）
 *   - 查询体：bool.filter.term.accountId + bool.must.multi_match(subject/from/bodyText)
 *   - 跨租户无共享索引；本 stub 仅占位，真实实现见 integration.ts
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
 * 真实 OpenSearch 适配器占位（Phase 2 / ADR-009 已在 src/integration/integration.ts 落地）。
 *
 * 真实实现：integration.OpenSearchIndex
 *   - index = `mail-${tid}`（per-tenant 物理索引）
 *   - search = bool.filter.term.accountId + bool.must.multi_match
 *   - 见 src/integration/integration.ts（fetch-based REST，与 Go src/search/index.go 对齐）
 *
 * PoC 编译期不引入外部依赖；运行时由 main.integration.ts 通过动态 import 装配真实适配器。
 */
export class OpenSearchIndex implements SearchIndex {
  private url: string;
  constructor(url: string) {
    this.url = url;
    console.warn(`[OpenSearchIndex] stub: ${url}（真实实现见 src/integration/integration.ts）`);
  }
  async index(_mail: CanonicalMail): Promise<void> {}
  async search(_t: string, _a: string, _q: string, _l: number): Promise<SearchHit[]> {
    return [];
  }
}
