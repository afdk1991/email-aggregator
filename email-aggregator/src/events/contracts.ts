/**
 * Kafka 事件契约（PoC 以内存总线实现，接口与真实 Kafka 一致）。
 * 所有事件带 partitionKey = accountId（单账户有序、可再均衡），
 * idempotencyKey 用于消费端去重（至少一次语义下防重复处理）。
 */

import type { CanonicalMail, SyncCursor } from "../model/canonical.ts";
import type { ProviderType } from "../model/connector.ts";

export type Topic =
  | "sync-tasks"
  | "mail-ingested"
  | "mail-index"
  | "notifications"
  | "audit";

export interface BaseEvent {
  /** 分区键：accountId */
  partitionKey: string;
  /** 幂等去重键 */
  idempotencyKey: string;
  /** 事件产生时间戳（ms） */
  ts: number;
  /**
   * 租户 ID（ADR-009）：全链路强制透传，用于逻辑多租户隔离。
   * 可选 + 默认值收敛于 DefaultTenantID，保证无租户头时与改造前等价（最小可逆）。
   */
  tenantId?: string;
}

/** sync-tasks：orchestrator → ingestion */
export interface SyncTaskEvent extends BaseEvent {
  topic: "sync-tasks";
  folder: string;
  cursor: SyncCursor;
  priority: number;
}

/** mail-ingested：ingestion → store / index / notify / audit */
export interface MailIngestedEvent extends BaseEvent {
  topic: "mail-ingested";
  accountId: string;
  canonicalMail: CanonicalMail;
  source: ProviderType;
}

/** mail-index：index-service → search-engine */
export interface MailIndexEvent extends BaseEvent {
  topic: "mail-index";
  docId: string;
  accountId: string;
  bodyText?: string;
}

/** notifications：notify → ws-push / webhook */
export interface NotificationEvent extends BaseEvent {
  topic: "notifications";
  kind: "new-mail" | "sync-state" | "error";
  preview?: string;
}

/** audit：各服务 → 审计存储 */
export interface AuditEvent extends BaseEvent {
  topic: "audit";
  actor: string;
  action: string;
  accountId: string;
  detail?: string;
}

export type AppEvent =
  | SyncTaskEvent
  | MailIngestedEvent
  | MailIndexEvent
  | NotificationEvent
  | AuditEvent;

export const TOPICS: Topic[] = [
  "sync-tasks",
  "mail-ingested",
  "mail-index",
  "notifications",
  "audit",
];
