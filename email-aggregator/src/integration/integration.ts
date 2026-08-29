/**
 * 真实基础设施适配器（生产骨架）—— 对齐 Go email-aggregator-go/src/integration/integration.go。
 *
 * 设计同构：所有外部依赖（pg / minio / @opensearch-project/opensearch / kafkajs）以
 * **动态 import** 加载，因此 PoC 默认零外部依赖即可运行；只有显式调用 Wire() 时才要求安装：
 *
 *   npm i kafkajs pg minio @opensearch-project/opensearch
 *   npm i -D @types/pg @types/minio
 *
 * 启用步骤（对齐 Go 的 `-tags integration`）：
 *   1) 安装上述依赖
 *   2) 在 main.ts 中以 `integration.Wire(cfg)` 替换 buildApp 的 InMemory 实现
 *   3) 启动 PG/MinIO/OpenSearch/Kafka 容器（见 deploy/docker-compose.yml）
 *
 * 所有适配器实现与 stub 完全相同的接口（MetadataStore / ContentStore / SearchIndex /
 * EventBus / Notifier），故替换编排/采集逻辑零改动。
 *
 * ── Phase 1+ 物理隔离（ADR-009）─────────────────────────────────────────────
 * 与 Go 当前 Phase 1（mail-<tid>-<accountId> 复合索引、对象存储 mail/<hash> 全局去重）不同，
 * TS PoC 直接落地 Phase 1+ 物理隔离，对应原 stub 中已声明的方向：
 *   - OpenSearch: mail-<tid> 单一租户索引（一租户一物理索引，跨账户在同一索引内以 accountId 过滤）
 *   - 对象存储:   tenant-<tid>/mail/<sha256>（每租户独立前缀，配合 KMS DEK 隔离）
 *   - KMS:        租户派生 KEK（envelope encryption），见 TenantKms 接口骨架
 *   - PG:         预留 tenant_id 列 + RLS（Citus 分布键切换为 tenant_id）
 */

import type { AppConfig } from "../config.ts";
import type { AppEvent, Topic } from "../events/contracts.ts";
import type { EventBus, Handler } from "../events/bus.ts";
import type { CanonicalMail, FolderName, SyncCursor } from "../model/canonical.ts";
import type { ContentStore, StoredContent } from "../store/content.ts";
import type { MetadataStore } from "../store/metadata.ts";
import type { CursorStore } from "../sync/orchestrator.ts";
import type { SearchHit, SearchIndex } from "../search/index.ts";
import type { Notifier, NotificationPayload, PushSink } from "../notify/ws.ts";
import { WsNotifier } from "../notify/ws.ts";
import { Resolve } from "../tenant/tenant.ts";
import { createHash } from "node:crypto";

// ───────────────────────────────────────────────────────────────────────────
// 配置
// ───────────────────────────────────────────────────────────────────────────

/**
 * IntegrationConfig 真实基础设施连接配置（通常由环境变量注入）。
 * 与 Go Config 同构；命名沿用 TS 驼峰约定。
 */
export interface IntegrationConfig {
  /** postgres://user:pass@host:5432/db */
  pgDsn: string;
  /** MinIO/S3 endpoint，如 localhost:9000 */
  objectEndpoint: string;
  objectBucket: string;
  objectAccessKey: string;
  objectSecretKey: string;
  objectSecure: boolean;
  /** http://localhost:9200 */
  openSearchAddr: string;
  /** OpenSearch 安全插件启用时需基础鉴权（如 admin） */
  openSearchUser: string;
  openSearchPass: string;
  /** ["localhost:9092"] */
  kafkaBrokers: string[];
  /**
   * KMS 端点（Phase 1+ 物理隔离：租户派生 KEK）。
   * 可选 —— 缺省回退到 InMemoryKms（仅 PoC，生产应接入 Vault / KMS / KMS-FOR-PCloud）。
   */
  kmsEndpoint?: string;
}

/** 从 AppConfig + 显式密钥构造 IntegrationConfig（典型入口） */
export function fromAppConfig(
  app: AppConfig,
  overrides: Partial<IntegrationConfig> = {},
): IntegrationConfig {
  return {
    pgDsn: app.pgDsn,
    objectEndpoint: app.minioEndpoint,
    objectBucket: overrides.objectBucket ?? "mail-content",
    objectAccessKey: overrides.objectAccessKey ?? "minioadmin",
    objectSecretKey: overrides.objectSecretKey ?? "minioadmin",
    objectSecure: overrides.objectSecure ?? app.minioEndpoint.startsWith("https"),
    openSearchAddr: app.opensearchUrl,
    openSearchUser: overrides.openSearchUser ?? "admin",
    openSearchPass: overrides.openSearchPass ?? "admin",
    kafkaBrokers: app.kafkaBrokers,
    kmsEndpoint: overrides.kmsEndpoint,
  };
}

// ───────────────────────────────────────────────────────────────────────────
// RealAdapters + Wire()
// ───────────────────────────────────────────────────────────────────────────

/**
 * RealAdapters 装配后的真实适配器集合（接口形态与 InMemory 版一致）。
 * 与 Go RealAdapters 同构：metadata + cursor 落在 PgMetadataStore 同一实例。
 */
export interface RealAdapters {
  bus: EventBus;
  metadata: MetadataStore;
  cursor: CursorStore;
  content: ContentStore;
  index: SearchIndex;
  notifier: Notifier;
  /** Phase 1+：每租户 KEK 接口（envelope encryption 用） */
  kms: TenantKms;
  /** 资源释放（Kafka 关闭、PG pool end、WS 关闭等） */
  close(): Promise<void>;
}

/**
 * Wire 按配置装配真实适配器（异步：含外部依赖动态 import + 连接握手）。
 * 与 Go integration.Wire 同构，但返回 Promise（动态 import 必须 async）。
 *
 * 失败语义：任一适配器构造失败 → 立即 reject，调用方应回退到 InMemory 装配。
 */
export async function Wire(cfg: IntegrationConfig): Promise<RealAdapters> {
  const pg = await NewPgMetadataStore(cfg.pgDsn);
  const obj = await NewObjectContentStore(
    cfg.objectEndpoint,
    cfg.objectBucket,
    cfg.objectAccessKey,
    cfg.objectSecretKey,
    cfg.objectSecure,
  );
  const osIdx = await NewOpenSearchIndex(
    cfg.openSearchAddr,
    cfg.openSearchUser,
    cfg.openSearchPass,
  );
  const hub = NewWsHub();
  const kms = NewTenantKms(cfg.kmsEndpoint);
  return {
    bus: NewKafkaAdapter(cfg.kafkaBrokers),
    metadata: pg,
    cursor: pg,
    content: obj,
    index: osIdx,
    notifier: hub,
    kms,
    close: async () => {
      await Promise.all([pg.close(), obj.close(), osIdx.close(), hub.close(), kms.close()]);
    },
  };
}

// ───────────────────────────────────────────────────────────────────────────
// PostgreSQL 元数据 + 游标仓储（MetadataStore + CursorStore）
// ───────────────────────────────────────────────────────────────────────────

/**
 * PgMetadataStore 真实 PG 适配器（动态 import `pg`）。
 *
 * 表结构（与 Go 对齐 + Phase 1+ tenant_id 强隔离）：
 *   mail_metadata(
 *     id TEXT PRIMARY KEY,              -- = CanonicalMail.idempotencyKey
 *     tenant_id TEXT NOT NULL,          -- ADR-009 逻辑隔离键 + Phase 1+ Citus 分布键
 *     account_id TEXT NOT NULL,
 *     provider TEXT, folder TEXT, subject TEXT, from_addr TEXT,
 *     body_text TEXT, internal_date BIGINT, size_bytes INT,
 *     raw_object_key TEXT, cursor_json JSONB, read BOOLEAN
 *   )
 *   account_sync_cursor(
 *     tenant_id TEXT, account_id TEXT, folder TEXT,
 *     cursor_json JSONB,
 *     PRIMARY KEY (tenant_id, account_id, folder)
 *   )
 *
 * Phase 1+ 物理隔离建议（#ts7 后续）：
 *   - 启用 PG Row-Level Security：CREATE POLICY tenant_isolation ON mail_metadata
 *     USING (tenant_id = current_setting('app.tenant_id'));
 *   - Citus 分布键从 account_id 切换为 tenant_id（一租户一 shard）
 */
export class PgMetadataStore implements MetadataStore, CursorStore {
  /** pg.Pool —— 类型擦除以避免静态依赖；运行时由 NewPgMetadataStore 注入 */
  private pool: { query: (text: string, values?: unknown[]) => Promise<{ rows: unknown[] }>; end: () => Promise<void> };
  private owned = false;

  private constructor(pool: PgPoolLike) {
    this.pool = pool;
    this.owned = true;
  }

  static async create(dsn: string): Promise<PgMetadataStore> {
    // 动态 import：pg 为可选 peerDep，缺失时 Wire() 会 reject
    // 类型擦除：mod 当作 any 处理，运行时形态校验靠 PgPoolLike
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const mod: any = await import("pg").catch((e) => {
      throw new Error(`[PgMetadataStore] 缺少 pg 依赖：npm i pg；原始错误: ${(e as Error).message}`);
    });
    const Pool = mod.Pool ?? mod.default?.Pool;
    if (typeof Pool !== "function") {
      throw new Error("[PgMetadataStore] pg 模块未导出 Pool，请检查 pg 版本");
    }
    const pool = new Pool({ connectionString: dsn }) as unknown as PgPoolLike;
    return new PgMetadataStore(pool);
  }

  async upsertMail(m: CanonicalMail): Promise<void> {
    const tid = Resolve(m.tenantId);
    await this.pool.query(
      `INSERT INTO mail_metadata
         (id, tenant_id, account_id, provider, folder, subject, from_addr,
          body_text, internal_date, size_bytes, raw_object_key, cursor_json)
       VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
       ON CONFLICT (id) DO NOTHING`,
      [
        m.idempotencyKey, tid, m.accountId, "imap", m.folder, m.subject,
        m.from[0]?.address ?? "", m.bodyText ?? "", m.internalDate, m.sizeBytes,
        m.rawObjectKey ?? "", JSON.stringify(m.cursor ?? {}),
      ],
    );
  }

  async getMail(tenantId: string, accountId: string, idempotencyKey: string): Promise<CanonicalMail | null> {
    const tid = Resolve(tenantId);
    const { rows } = await this.pool.query(
      `SELECT id, tenant_id, account_id, folder, subject, from_addr, body_text,
              internal_date, size_bytes, raw_object_key, cursor_json
       FROM mail_metadata
       WHERE id=$1 AND account_id=$2 AND tenant_id=$3`,
      [idempotencyKey, accountId, tid],
    );
    if (rows.length === 0) return null;
    return rowToMail(rows[0] as MailRow);
  }

  async listMails(tenantId: string, accountId: string, folder: FolderName, limit: number): Promise<CanonicalMail[]> {
    const tid = Resolve(tenantId);
    const { rows } = await this.pool.query(
      `SELECT id, tenant_id, account_id, folder, subject, from_addr, body_text,
              internal_date, size_bytes, raw_object_key, cursor_json
       FROM mail_metadata
       WHERE account_id=$1 AND folder=$2 AND tenant_id=$3
       ORDER BY internal_date DESC LIMIT $4`,
      [accountId, folder, tid, limit],
    );
    return rows.map((r) => rowToMail(r as MailRow));
  }

  async count(tenantId: string, accountId: string): Promise<number> {
    const tid = Resolve(tenantId);
    const { rows } = await this.pool.query(
      `SELECT count(*)::int AS n FROM mail_metadata WHERE account_id=$1 AND tenant_id=$2`,
      [accountId, tid],
    );
    return (rows[0] as { n: number }).n ?? 0;
  }

  // ---- CursorStore 实现（与 InMemoryMetadataStore 同构：按 tenant+account+folder 持久化） ----
  async get(tenantId: string, accountId: string, folder: string): Promise<SyncCursor | null> {
    const tid = Resolve(tenantId);
    const { rows } = await this.pool.query(
      `SELECT cursor_json FROM account_sync_cursor
       WHERE account_id=$1 AND folder=$2 AND tenant_id=$3`,
      [accountId, folder, tid],
    );
    if (rows.length === 0) return null;
    return (rows[0] as { cursor_json: SyncCursor }).cursor_json ?? null;
  }

  async put(tenantId: string, accountId: string, folder: string, cursor: SyncCursor): Promise<void> {
    const tid = Resolve(tenantId);
    await this.pool.query(
      `INSERT INTO account_sync_cursor (tenant_id, account_id, folder, cursor_json)
       VALUES ($1,$2,$3,$4)
       ON CONFLICT (tenant_id, account_id, folder) DO UPDATE SET cursor_json = EXCLUDED.cursor_json`,
      [tid, accountId, folder, JSON.stringify(cursor)],
    );
  }

  async close(): Promise<void> {
    if (this.owned) await this.pool.end();
    this.owned = false;
  }
}

/** pg.Pool 的最小类型签名（仅我们用到的两个方法） */
interface PgPoolLike {
  query(text: string, values?: unknown[]): Promise<{ rows: unknown[] }>;
  end(): Promise<void>;
}

interface MailRow {
  id: string;
  tenant_id: string;
  account_id: string;
  folder: string;
  subject: string;
  from_addr: string;
  body_text: string | null;
  internal_date: number;
  size_bytes: number;
  raw_object_key: string | null;
  cursor_json: SyncCursor | null;
}

function rowToMail(r: MailRow): CanonicalMail {
  return {
    idempotencyKey: r.id,
    tenantId: r.tenant_id,
    accountId: r.account_id,
    folder: r.folder as FolderName,
    messageId: r.id, // PoC：messageId 复用 idempotencyKey；真实场景应单独解析
    from: r.from_addr ? [{ address: r.from_addr }] : [],
    to: [], cc: [],
    subject: r.subject,
    bodyText: r.body_text ?? undefined,
    attachments: [],
    flags: { seen: false, flagged: false, answered: false, deleted: false },
    internalDate: Number(r.internal_date) || 0,
    sizeBytes: Number(r.size_bytes) || 0,
    cursor: r.cursor_json ?? {},
    rawObjectKey: r.raw_object_key ?? undefined,
  };
}

// ───────────────────────────────────────────────────────────────────────────
// 对象存储内容仓储（ContentStore，MinIO/S3 兼容，内容寻址去重）
// Phase 1+ 物理隔离：前缀 tenant-<tid>/mail/<sha256>
// ───────────────────────────────────────────────────────────────────────────

/**
 * ObjectContentStore 真实对象存储适配器（动态 import `minio`）。
 *
 * Phase 1+ 物理隔离：每租户独立前缀 `tenant-<tid>/mail/<sha256>`，
 * 配合 KMS 租户派生 KEK（envelope encryption）实现加密隔离。
 * 与 Go 当前 Phase 1（mail/<sha256> 全局去重）不同——TS PoC 直接落地 Phase 1+。
 *
 * 注：内容寻址去重现在变为「租户内去重」——同一内容在不同租户下各存一份，
 * 这是 Phase 1+ 物理隔离的代价（换取加密 + 合规隔离）。
 */
export class ObjectContentStore implements ContentStore {
  private client: MinioClientLike;
  private bucket: string;

  private constructor(client: MinioClientLike, bucket: string) {
    this.client = client;
    this.bucket = bucket;
  }

  static async create(
    endpoint: string,
    bucket: string,
    accessKey: string,
    secretKey: string,
    secure: boolean,
  ): Promise<ObjectContentStore> {
    // 动态 import：minio 为可选 peerDep
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const mod: any = await import("minio").catch((e) => {
      throw new Error(`[ObjectContentStore] 缺少 minio 依赖：npm i minio；原始错误: ${(e as Error).message}`);
    });
    const Client = mod.Client ?? mod.default?.Client;
    if (typeof Client !== "function") {
      throw new Error("[ObjectContentStore] minio 模块未导出 Client");
    }
    const client = new Client({
      endPoint: endpoint.split(":")[0],
      port: Number(endpoint.split(":")[1] ?? (secure ? 443 : 80)),
      useSSL: secure,
      accessKey,
      secretKey,
    }) as unknown as MinioClientLike;
    return new ObjectContentStore(client, bucket);
  }

  /**
   * Put 内容寻址写入（已存在则跳过）。
   * @param content 二进制正文
   * @param tenantId 用于 Phase 1+ 前缀隔离（默认 DefaultTenantID）
   */
  async put(content: Buffer, tenantId?: string): Promise<StoredContent> {
    const tid = Resolve(tenantId);
    const hash = sha256Hex(content);
    const objectKey = `tenant-${tid}/mail/${hash}`;
    // 幂等：先 stat，已存在直接返回（租户内去重）
    try {
      await this.client.statObject(this.bucket, objectKey);
      return { objectKey, contentHash: hash, size: content.length };
    } catch {
      // 不存在，继续写入
    }
    await this.client.putObject(this.bucket, objectKey, content, content.length, {
      "Content-Type": "application/octet-stream",
    });
    return { objectKey, contentHash: hash, size: content.length };
  }

  async get(objectKey: string): Promise<Buffer> {
    const stream = await this.client.getObject(this.bucket, objectKey);
    const chunks: Buffer[] = [];
    for await (const chunk of stream) chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    return Buffer.concat(chunks);
  }

  async close(): Promise<void> {
    /* minio-go 客户端无显式 close；保持空实现以对齐接口 */
  }
}

/** minio.Client 的最小类型签名 */
interface MinioClientLike {
  statObject(bucket: string, key: string): Promise<unknown>;
  putObject(bucket: string, key: string, body: Buffer, size: number, meta: Record<string, string>): Promise<unknown>;
  getObject(bucket: string, key: string): Promise<AsyncIterable<Buffer | Uint8Array>>;
}

function sha256Hex(buf: Buffer): string {
  return createHash("sha256").update(buf).digest("hex");
}

// ───────────────────────────────────────────────────────────────────────────
// OpenSearch 检索索引（SearchIndex，Phase 1+ 物理隔离：mail-<tid> 单一租户索引）
// ───────────────────────────────────────────────────────────────────────────

/**
 * OpenSearchIndex 真实索引适配器（fetch-based REST，无强依赖客户端库）。
 *
 * Phase 1+ 物理隔离：index = `mail-<tenantId>`（一租户一物理索引），
 * 跨账户在同一索引内以 accountId 过滤（避免 mail-<tid>-<acc> 索引膨胀）。
 *
 * 与 Go 当前 Phase 1（mail-<tid>-<accountId> 复合索引）不同——TS PoC 直接落地 Phase 1+，
 * 对应 src/search/index.ts stub 中已声明的 `mail-<tid>` 方向。
 *
 * 实现策略：直接走 OpenSearch REST API（fetch），无需 `@opensearch-project/opensearch` 客户端
 * —— PoC 零依赖运行；生产可替换为官方客户端以获得连接池 / 重试 / 类型提示。
 */
export class OpenSearchIndex implements SearchIndex {
  private addr: string;
  private auth: string;

  private constructor(addr: string, user: string, pass: string) {
    this.addr = addr.replace(/\/$/, "");
    this.auth = user && pass ? "Basic " + Buffer.from(`${user}:${pass}`).toString("base64") : "";
  }

  static async create(addr: string, user: string, pass: string): Promise<OpenSearchIndex> {
    // 连通性探测（不致命：失败仅 warn，真实写入时再报错）
    const inst = new OpenSearchIndex(addr, user, pass);
    try {
      const ok = await inst.ping();
      if (!ok) console.warn(`[OpenSearchIndex] ping failed: ${addr}（继续装配，首次写入时会重试）`);
    } catch (e) {
      console.warn(`[OpenSearchIndex] ping error: ${(e as Error).message}`);
    }
    return inst;
  }

  private async ping(): Promise<boolean> {
    try {
      const r = await fetch(`${this.addr}/_cluster/health`);
      return r.ok;
    } catch {
      return false;
    }
  }

  /** Phase 1+：索引名 = mail-<tenantId>，跨账户在同一索引内以 accountId 过滤 */
  private indexName(tenantId: string): string {
    return `mail-${sanitizeIndex(Resolve(tenantId))}`;
  }

  async index(mail: CanonicalMail): Promise<void> {
    const tid = Resolve(mail.tenantId);
    const doc = {
      tenantId: tid,
      accountId: mail.accountId,
      idempotencyKey: mail.idempotencyKey,
      subject: mail.subject,
      from: mail.from.map((f) => f.address).join(", "),
      bodyText: mail.bodyText ?? "",
      internalDate: mail.internalDate,
    };
    const idx = this.indexName(tid);
    const r = await fetch(`${this.addr}/${idx}/_doc/${encodeURIComponent(mail.idempotencyKey)}`, {
      method: "POST",
      headers: this.headers(),
      body: JSON.stringify(doc),
    });
    if (!r.ok) {
      // 索引不存在时自动创建（PoC：无 index template）
      if (r.status === 404) {
        await this.ensureIndex(idx);
        const r2 = await fetch(`${this.addr}/${idx}/_doc/${encodeURIComponent(mail.idempotencyKey)}`, {
          method: "POST", headers: this.headers(), body: JSON.stringify(doc),
        });
        if (!r2.ok) throw new Error(`opensearch index(2): ${r2.status} ${await r2.text()}`);
        return;
      }
      throw new Error(`opensearch index: ${r.status} ${await r.text()}`);
    }
  }

  async search(tenantId: string, accountId: string, query: string, limit: number): Promise<SearchHit[]> {
    const tid = Resolve(tenantId);
    const idx = this.indexName(tid);
    const body = {
      size: limit,
      query: {
        bool: {
          must: [
            { term: { accountId } },
            { multi_match: { query, fields: ["subject", "from", "bodyText"] } },
          ],
        },
      },
      sort: [{ internalDate: "desc" }],
    };
    const r = await fetch(`${this.addr}/${idx}/_search`, {
      method: "POST", headers: this.headers(), body: JSON.stringify(body),
    });
    if (!r.ok) {
      if (r.status === 404) return []; // 索引不存在视为空结果（租户从未写入）
      throw new Error(`opensearch search: ${r.status} ${await r.text()}`);
    }
    const json = (await r.json()) as { hits?: { hits?: Array<{ _source?: OpenSearchDoc }> } };
    return (json.hits?.hits ?? [])
      .map((h) => h._source)
      .filter((d): d is OpenSearchDoc => !!d)
      .filter((d) => !!d.idempotencyKey && !!d.accountId)
      .map((d) => ({
        idempotencyKey: d.idempotencyKey as string,
        tenantId: tid,
        accountId: d.accountId as string,
        subject: d.subject ?? "",
        from: d.from ?? "",
        preview: (d.bodyText ?? "").slice(0, 80),
        internalDate: Number(d.internalDate) || 0,
      }));
  }

  private async ensureIndex(idx: string): Promise<void> {
    const r = await fetch(`${this.addr}/${idx}`, { method: "PUT", headers: this.headers() });
    if (!r.ok && r.status !== 400) {
      // 400 = 已存在；其他状态码视为成功（PoC 宽容策略）
    }
  }

  private headers(): Record<string, string> {
    return {
      "Content-Type": "application/json",
      ...(this.auth ? { Authorization: this.auth } : {}),
    };
  }

  async close(): Promise<void> {
    /* HTTP fetch 无持久连接需关闭 */
  }
}

interface OpenSearchDoc {
  tenantId?: string;
  accountId?: string;
  idempotencyKey?: string;
  subject?: string;
  from?: string;
  bodyText?: string;
  internalDate?: number;
}

/** 索引名小写化 + 非法字符替换（对齐 Go sanitizeIndex） */
function sanitizeIndex(id: string): string {
  return id.toLowerCase().replace(/[^a-z0-9_-]/g, "_");
}

// ───────────────────────────────────────────────────────────────────────────
// Kafka 事件总线（EventBus，kafkajs）
// ───────────────────────────────────────────────────────────────────────────

/**
 * KafkaAdapter 真实 Kafka 适配器（动态 import `kafkajs`）。
 * 与 Go segmentio/kafka-go 同构：生产者用 Producer，消费者按 group 起 Consumer。
 *
 * 幂等去重：消费端基于 idempotencyKey 自行去重（与 InMemoryEventBus 一致）。
 * 顺序保证：partitionKey = accountId → 同账户单分区有序。
 */
export class KafkaAdapter implements EventBus {
  private kafka: KafkaLike | null = null;
  private producer: KafkaProducerLike | null = null;
  private consumers = new Map<string, KafkaConsumerLike>();
  private handlers = new Map<Topic, Handler>();
  private brokers: string[];
  private running = false;

  /** 同步构造（与 Go NewKafkaAdapter 同构：仅记 brokers，真实连接在 ensure() 懒触发） */
  constructor(brokers: string[]) {
    this.brokers = brokers;
  }

  static async create(brokers: string[]): Promise<KafkaAdapter> {
    return new KafkaAdapter(brokers);
  }

  /** 懒初始化 kafka + producer（首次 publish/subscribe 时触发） */
  private async ensure(): Promise<void> {
    if (this.kafka) return;
    // 动态 import：kafkajs 为可选 peerDep
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const mod: any = await import("kafkajs").catch((e) => {
      throw new Error(`[KafkaAdapter] 缺少 kafkajs 依赖：npm i kafkajs；原始错误: ${(e as Error).message}`);
    });
    const Kafka = mod.Kafka ?? mod.default?.Kafka;
    if (typeof Kafka !== "function") {
      throw new Error("[KafkaAdapter] kafkajs 模块未导出 Kafka");
    }
    this.kafka = new Kafka({ brokers: this.brokers }) as unknown as KafkaLike;
    this.producer = this.kafka.producer();
    await this.producer.connect();
  }

  async publish(event: AppEvent): Promise<void> {
    await this.ensure();
    if (!this.producer) throw new Error("kafka producer not connected");
    await this.producer.send({
      topic: event.topic,
      messages: [{
        key: event.partitionKey,
        value: JSON.stringify(event),
        // ADR-009：在 header 中也带 tenantId，便于跨服务观测/路由
        headers: event.tenantId ? { tenantId: event.tenantId } : undefined,
      }],
    });
  }

  subscribe(topic: Topic, handler: Handler): void {
    this.handlers.set(topic, handler);
    // 异步起消费者（不阻塞调用方）
    void this.startConsumer(topic).catch((e) => {
      console.error(`[KafkaAdapter] consumer start failed on ${topic}:`, (e as Error).message);
    });
  }

  private async startConsumer(topic: Topic): Promise<void> {
    await this.ensure();
    if (!this.kafka) throw new Error("kafka client not initialized");
    const consumer = this.kafka.consumer({ groupId: `email-aggregator-${topic}` });
    await consumer.connect();
    await consumer.subscribe({ topic, fromBeginning: false });
    this.consumers.set(topic, consumer);
    await consumer.run({
      eachMessage: async ({ message }: { message: { key?: Buffer | string; value?: Buffer | string } }) => {
        const handler = this.handlers.get(topic);
        if (!handler) return;
        try {
          const raw = typeof message.value === "string" ? message.value : message.value?.toString("utf8") ?? "{}";
          const ev = JSON.parse(raw) as AppEvent;
          await handler(ev);
        } catch (e) {
          console.error(`[KafkaAdapter] handler failed on ${topic}:`, (e as Error).message);
        }
      },
    });
  }

  async start(): Promise<void> {
    this.running = true;
  }

  async stop(): Promise<void> {
    this.running = false;
    await Promise.all([...this.consumers.values()].map((c) => c.disconnect()));
    this.consumers.clear();
    if (this.producer) await this.producer.disconnect();
  }

  async close(): Promise<void> {
    await this.stop();
  }
}

/** kafkajs 最小类型签名 */
interface KafkaLike {
  producer(): KafkaProducerLike;
  consumer(opts: { groupId: string }): KafkaConsumerLike;
}
interface KafkaProducerLike {
  connect(): Promise<void>;
  send(msg: { topic: string; messages: Array<{ key?: string; value: string; headers?: Record<string, string> }> }): Promise<void>;
  disconnect(): Promise<void>;
}
interface KafkaConsumerLike {
  connect(): Promise<void>;
  subscribe(opts: { topic: string; fromBeginning: boolean }): Promise<void>;
  run(opts: { eachMessage: (payload: { message: { key?: Buffer | string; value?: Buffer | string } }) => Promise<void> }): Promise<void>;
  disconnect(): Promise<void>;
}

// ───────────────────────────────────────────────────────────────────────────
// WebSocket 实时通知（Notifier）—— 复用已有 WsNotifier（零依赖内置实现）
// ───────────────────────────────────────────────────────────────────────────

/**
 * WsHub 实时 WebSocket 通知中心。
 *
 * PoC 选择：复用 src/notify/ws.ts 中的 WsNotifier（基于 Node 内置 http+crypto，
 * 零外部依赖即可工作）—— 与 Go integration.go 使用 gorilla/websocket 不同，
 * TS 版不需要 `ws` 包；如需更强大的帧控制（permessage-deflate / 流式）再切换。
 *
 * 租户隔离（ADR-009）：sink 与 publish 均以 (tenantId, accountId) 复合键路由。
 */
export interface WsHub extends Notifier {
  /** 挂载到已有 HTTP 服务，使 REST 与 WS 同端口 */
  attach(server: import("node:http").Server): void;
  /** 单独监听端口（无 HTTP 服务复用场景） */
  listen(port: number): Promise<void>;
  /** 释放资源（关闭底层 HTTP/WS） */
  close(): void;
}

export function NewWsHub(): WsHub {
  // WsNotifier 已实现 Notifier 接口 + attach/listen + close，直接包装返回
  const inner = new WsNotifier();
  return inner as unknown as WsHub;
}

// ───────────────────────────────────────────────────────────────────────────
// Phase 1+ 物理隔离：KMS 租户派生 KEK 接口骨架（#ts7）
// ───────────────────────────────────────────────────────────────────────────

/**
 * TenantKms 租户级密钥管理（envelope encryption）。
 *
 * Phase 1+ 物理隔离的核心：每租户独立 KEK（Key Encryption Key），DEK（Data Encryption Key）
 * 由 KEK 派生 + 加密后存储。保证即使对象存储被越权访问，跨租户密文也无法解密。
 *
 * 生产落地建议：
 *   - AWS KMS / GCP KMS / 阿里云 KMS / Vault Transit：每租户独立 keyring / key
 *   - KEK 永不离开 HSM；DEK 加密后返回，本地短暂缓存（TTL ≤ 5min）
 *   - 密钥轮转：每租户维护 activeKeyId + 历史 keyId（解密时按 keyId 路由）
 *
 * PoC 实现：InMemoryTenantKms（仅占位，DEK = sha256(KEK + objectKey) 派生，无真实加密）。
 */
export interface TenantKms {
  /**
   * 生成 / 获取租户的当前 active KEK ID。
   * 返回值写入对象元数据，用于解密时路由到正确密钥。
   */
  activeKeyId(tenantId: string): string;

  /**
   * 派生 DEK（数据加密密钥）。
   * @param tenantId 租户
   * @param context AAD（附加认证数据）—— 通常含 objectKey / accountId / internalDate
   * @returns { plaintextDEK, encryptedDEK, keyId } —— plaintextDEK 短暂缓存用于加解密，
   *          encryptedDEK + keyId 持久化到对象元数据
   */
  deriveDEK(tenantId: string, context: Buffer): Promise<{ plaintextDEK: Buffer; encryptedDEK: Buffer; keyId: string }>;

  /**
   * 解密 DEK（用租户 KEK 还原 plaintextDEK）。
   * 失败语义：跨租户访问 → KEK 不匹配 → 解密失败 → 抛错。
   */
  decryptDEK(tenantId: string, encryptedDEK: Buffer, keyId: string): Promise<Buffer>;

  /** 释放资源（关闭 HSM 连接等） */
  close(): Promise<void>;
}

export function NewTenantKms(endpoint?: string): TenantKms {
  if (!endpoint) return new InMemoryTenantKms();
  // 生产应动态 import 对应 KMS SDK；PoC 占位
  console.warn(`[TenantKms] endpoint=${endpoint} 未接入真实 KMS SDK，回退到 InMemory`);
  return new InMemoryTenantKms();
}

/**
 * InMemoryTenantKms PoC 实现。
 *
 * 注意：**未做真实加密** —— KEK/DEK 均为派生哈希，仅用于演示 envelope encryption 的接口形态。
 * 生产必须替换为真实 KMS（HSM 后端）。
 */
class InMemoryTenantKms implements TenantKms {
  private keks = new Map<string, string>(); // tenantId -> KEK (hex)
  private keyIds = new Map<string, string>(); // tenantId -> activeKeyId

  activeKeyId(tenantId: string): string {
    const tid = Resolve(tenantId);
    if (!this.keyIds.has(tid)) {
      this.keyIds.set(tid, `kek-${tid}-${Date.now()}`);
      this.keks.set(tid, sha256Hex(Buffer.from(`kek-seed-${tid}`)));
    }
    return this.keyIds.get(tid)!;
  }

  async deriveDEK(tenantId: string, context: Buffer): Promise<{ plaintextDEK: Buffer; encryptedDEK: Buffer; keyId: string }> {
    const tid = Resolve(tenantId);
    const keyId = this.activeKeyId(tid);
    const kek = this.keks.get(tid)!;
    const plaintextDEK = Buffer.from(sha256Hex(Buffer.concat([Buffer.from(kek), context])), "hex");
    // PoC "加密"：XOR with KEK prefix（演示用，非真实加密）
    const encryptedDEK = Buffer.from(plaintextDEK);
    for (let i = 0; i < encryptedDEK.length; i++) {
      encryptedDEK[i] ^= Buffer.from(kek, "hex")[i % 32] ?? 0;
    }
    return { plaintextDEK, encryptedDEK, keyId };
  }

  async decryptDEK(tenantId: string, encryptedDEK: Buffer, keyId: string): Promise<Buffer> {
    const tid = Resolve(tenantId);
    const kek = this.keks.get(tid);
    if (!kek || this.keyIds.get(tid) !== keyId) {
      throw new Error(`KMS: tenant ${tid} 无法解密 keyId=${keyId}（跨租户访问或密钥已轮转）`);
    }
    // 反向 XOR
    const out = Buffer.from(encryptedDEK);
    for (let i = 0; i < out.length; i++) {
      out[i] ^= Buffer.from(kek, "hex")[i % 32] ?? 0;
    }
    return out;
  }

  async close(): Promise<void> { /* noop */ }
}

// ───────────────────────────────────────────────────────────────────────────
// 工厂函数（与 Go NewXxx 同构）
// ───────────────────────────────────────────────────────────────────────────

export async function NewPgMetadataStore(dsn: string): Promise<PgMetadataStore> {
  return PgMetadataStore.create(dsn);
}

export async function NewObjectContentStore(
  endpoint: string, bucket: string, accessKey: string, secretKey: string, secure: boolean,
): Promise<ObjectContentStore> {
  return ObjectContentStore.create(endpoint, bucket, accessKey, secretKey, secure);
}

export async function NewOpenSearchIndex(addr: string, user: string, pass: string): Promise<OpenSearchIndex> {
  return OpenSearchIndex.create(addr, user, pass);
}

export function NewKafkaAdapter(brokers: string[]): KafkaAdapter {
  // KafkaAdapter 的真实连接在首次 publish/subscribe 时懒触发（与 Go 不同——Go 在构造时即创建 Writer）
  // 这样可以容忍 Kafka 暂时不可达时仍能装配（启动顺序不阻塞）
  return new KafkaAdapter(brokers);
}
