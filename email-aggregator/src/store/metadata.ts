/**
 * 元数据仓储。元数据（发件人/主题/时间/文件夹/游标）存 PostgreSQL，
 * 后续按 tenant_id + account_id 分片（ADR-009）。PoC 以 InMemoryMetadataStore 实现，
 * PgMetadataStore 为真实集成 stub（接口一致）。
 *
 * 与 Go 实现对齐：email-aggregator-go/src/store/metadata.go（tenantId 首参 + Resolve 收敛）。
 */

import type { CanonicalMail, FolderName, SyncCursor } from "../model/canonical.ts";
import type { CursorStore } from "../sync/orchestrator.ts";
import { Resolve } from "../tenant/tenant.ts";

export interface MetadataStore {
  upsertMail(m: CanonicalMail): Promise<void>;
  getMail(tenantId: string, accountId: string, idempotencyKey: string): Promise<CanonicalMail | null>;
  listMails(tenantId: string, accountId: string, folder: FolderName, limit: number): Promise<CanonicalMail[]>;
  count(tenantId: string, accountId: string): Promise<number>;
}

export class InMemoryMetadataStore implements MetadataStore, CursorStore {
  // `${tenantId}|${idempotencyKey}` -> mail（防跨租户同键碰撞）
  private byKey = new Map<string, CanonicalMail>();
  // `${tenantId}|${accountId}` -> mails（租户隔离的主索引）
  private byAccount = new Map<string, CanonicalMail[]>();
  // `${tenantId}|${accountId}|${folder}` -> cursor
  private cursors = new Map<string, SyncCursor>();

  async upsertMail(m: CanonicalMail): Promise<void> {
    const tid = Resolve(m.tenantId);
    const mWithTid: CanonicalMail = { ...m, tenantId: tid };
    const k = `${tid}|${m.idempotencyKey}`;
    this.byKey.set(k, mWithTid);
    const acc = `${tid}|${m.accountId}`;
    const list = this.byAccount.get(acc) ?? [];
    if (!list.find((x) => x.idempotencyKey === m.idempotencyKey)) list.push(mWithTid);
    this.byAccount.set(acc, list);
  }

  async getMail(tenantId: string, accountId: string, idempotencyKey: string): Promise<CanonicalMail | null> {
    const tid = Resolve(tenantId);
    const m = this.byKey.get(`${tid}|${idempotencyKey}`);
    return m && m.accountId === accountId ? m : null;
  }

  async listMails(tenantId: string, accountId: string, folder: FolderName, limit: number): Promise<CanonicalMail[]> {
    const tid = Resolve(tenantId);
    const list = this.byAccount.get(`${tid}|${accountId}`) ?? [];
    return list.filter((m) => m.folder === folder).slice(-limit).reverse();
  }

  async count(tenantId: string, accountId: string): Promise<number> {
    const tid = Resolve(tenantId);
    return (this.byAccount.get(`${tid}|${accountId}`) ?? []).length;
  }

  // ---- CursorStore 实现（落在元数据层，按租户+账户+文件夹持久化） ----
  async get(tenantId: string, accountId: string, folder: string): Promise<SyncCursor | null> {
    return this.cursors.get(`${Resolve(tenantId)}|${accountId}|${folder}`) ?? null;
  }
  async put(tenantId: string, accountId: string, folder: string, cursor: SyncCursor): Promise<void> {
    this.cursors.set(`${Resolve(tenantId)}|${accountId}|${folder}`, cursor);
  }
}

/**
 * @deprecated STUB — 未实现。仅作接口文档保留。
 * 真实 PG 适配器落地时替换为（按 tenant_id + account_id 分片）：
 *   INSERT ... ON CONFLICT (tenant_id, idempotency_key) DO NOTHING;
 *   -- 游标表 account_sync_cursor(tenant_id, account_id, folder, cursor_json)
 * Go 版真实实现见 integration/integration.go 中的 PgMetadataStore。
 */
export class PgMetadataStore implements MetadataStore {
  private dsn: string;
  constructor(dsn: string) {
    this.dsn = dsn;
    console.warn(`[PgMetadataStore] stub: 未连接 ${dsn}`);
  }
  async upsertMail(_m: CanonicalMail): Promise<void> {
    throw new Error("PgMetadataStore.upsertMail not implemented");
  }
  async getMail(_t: string, _a: string, _k: string): Promise<CanonicalMail | null> {
    return null;
  }
  async listMails(_t: string, _a: string, _f: FolderName, _l: number): Promise<CanonicalMail[]> {
    return [];
  }
  async count(_t: string, _a: string): Promise<number> {
    return 0;
  }
}
