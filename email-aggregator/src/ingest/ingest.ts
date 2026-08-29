/**
 * 采集服务（ingest）：消费 sync-tasks → 按 UID 拉取完整 MIME →
 * 规范化成 CanonicalMail → 内容寻址落对象存储 → 元数据落 PG → 发布 mail-ingested。
 *
 * 至少一次 + 幂等：mail-ingested 的 idempotencyKey = 邮件去重键；
 * 下游（store/index/notify）按此去重，重复事件安全。
 */

import type { Connector, AccountCredential, Session } from "../model/connector.ts";
import { idempotencyKey, type CanonicalMail, type FolderName } from "../model/canonical.ts";
import type { EventBus } from "../events/bus.ts";
import type { SyncTaskEvent, MailIngestedEvent } from "../events/contracts.ts";
import { parseRawMessage } from "../connector/imap.ts";
import type { MetadataStore } from "../store/metadata.ts";
import type { ContentStore } from "../store/content.ts";
import { Resolve } from "../tenant/tenant.ts";

export interface IngestDeps {
  connector: Connector;
  bus: EventBus;
  metadata: MetadataStore;
  content: ContentStore;
}

export class IngestService {
  private sessions = new Map<string, Session>();

  private deps: IngestDeps;
  constructor(deps: IngestDeps) {
    this.deps = deps;
  }

  start(): void {
    this.deps.bus.subscribe("sync-tasks", (e) => this.onTask(e as SyncTaskEvent));
  }

  private async sessionFor(accountId: string): Promise<Session> {
    let s = this.sessions.get(accountId);
    if (!s) {
      const cred: AccountCredential = {
        accountId,
        providerType: this.deps.connector.providerType,
        authMethod: "oauth2",
        accessToken: "demo",
      };
      s = await this.deps.connector.connect(cred);
      this.sessions.set(accountId, s);
    }
    return s;
  }

  private async onTask(task: SyncTaskEvent): Promise<void> {
    const tid = Resolve(task.tenantId); // ADR-009：从 sync-tasks 透传的租户 ID
    const session = await this.sessionFor(task.partitionKey);
    const raw = await this.deps.connector.fetchMessage(session, task.folder, taskUid(task));
    const parsed = parseRawMessage(raw);

    const mail: CanonicalMail = {
      idempotencyKey: idempotencyKey(task.partitionKey, task.folder as FolderName, parsed.messageId, tid),
      tenantId: tid,
      accountId: task.partitionKey,
      folder: task.folder as FolderName,
      messageId: parsed.messageId,
      from: [{ address: parsed.from }],
      to: parsed.to.map((a) => ({ address: a })),
      cc: [],
      subject: parsed.subject,
      bodyText: parsed.bodyText,
      attachments: [],
      flags: {
        seen: raw.flags?.seen ?? false,
        flagged: raw.flags?.flagged ?? false,
        answered: raw.flags?.answered ?? false,
        deleted: raw.flags?.deleted ?? false,
      },
      internalDate: parsed.internalDate,
      sizeBytes: raw.sizeBytes,
      cursor: task.cursor,
    };

    // 内容寻址落对象存储（相同内容只存一份）
    const stored = await this.deps.content.put(Buffer.from(raw.mime));
    mail.rawObjectKey = stored.objectKey;

    // 元数据落 PG（按 tenantId + accountId 分片）
    await this.deps.metadata.upsertMail(mail);

    const ev: MailIngestedEvent = {
      topic: "mail-ingested",
      partitionKey: mail.accountId,
      idempotencyKey: mail.idempotencyKey,
      ts: Date.now(),
      tenantId: tid,
      accountId: mail.accountId,
      canonicalMail: mail,
      source: this.deps.connector.providerType,
    };
    await this.deps.bus.publish(ev);
  }
}

/**
 * sync-tasks 的 idempotencyKey 形如 `tid|acc|task|<uid>|<modseq>`，反解 uid。
 * 兼容旧格式 `acc|task|<uid>|<modseq>`（无租户前缀，uid 在 parts[2]）。
 */
function taskUid(task: SyncTaskEvent): string {
  const parts = task.idempotencyKey.split("|");
  // 新格式: [tid, acc, "task", uid, modseq] → uid 在 parts[3]
  // 旧格式: [acc, "task", uid, modseq]     → uid 在 parts[2]
  // 判据：parts[1] === "task" → 旧格式；parts[2] === "task" → 新格式
  if (parts[1] === "task") return parts[2] ?? task.folder;
  if (parts[2] === "task") return parts[3] ?? task.folder;
  return task.folder;
}
