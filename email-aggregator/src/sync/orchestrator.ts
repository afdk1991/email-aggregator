/**
 * 同步编排器：驱动单账户的状态机，把"连接器事件"翻译为总线事件。
 *
 * 职责边界：
 *  - 负责：授权流转、初始全量、增量触发、限流退避、游标持久化、发 sync-tasks。
 *  - 不负责：拉取邮件正文并落库（那由 ingest 服务消费 sync-tasks 完成）。
 *
 * 至少一次 + 幂等：sync-tasks 由 ingest 端按 idempotencyKey 去重，重复派发安全。
 */

import type { Connector } from "../model/connector.ts";
import type { SyncCursor } from "../model/canonical.ts";
import type { EventBus } from "../events/bus.ts";
import type { AppEvent, SyncTaskEvent } from "../events/contracts.ts";
import { SyncStateMachine, type SyncState } from "./stateMachine.ts";
import type { SealedCredential } from "../security/vault.ts";
import { Resolve } from "../tenant/tenant.ts";

export interface CursorStore {
  get(tenantId: string, accountId: string, folder: string): Promise<SyncCursor | null>;
  put(tenantId: string, accountId: string, folder: string, cursor: SyncCursor): Promise<void>;
}

export interface TokenSealer {
  seal(plaintextToken: string): Promise<SealedCredential>;
}

export interface OrchestratorDeps {
  connector: Connector;
  bus: EventBus;
  cursors: CursorStore;
  sealer: TokenSealer;
  /** 上游 429 退避上限（ms） */
  backoffMaxMs?: number;
  /** 租户 ID（ADR-009）：空值收敛于 DefaultTenantID；贯穿 sync-tasks / audit 事件与游标读写 */
  tenantId?: string;
}

const FOLDER = "INBOX";

export class SyncOrchestrator {
  private fsm = new SyncStateMachine();
  private sessions = new Map<string, Awaited<ReturnType<Connector["connect"]>>>();
  private deps: OrchestratorDeps;
  private stopFlags = new Set<string>();
  private backoffMaxMs: number;
  private tenantId: string;

  constructor(deps: OrchestratorDeps) {
    this.deps = deps;
    this.backoffMaxMs = deps.backoffMaxMs ?? 60000;
    this.tenantId = Resolve(deps.tenantId);
  }

  getState(accountId: string): SyncState {
    return this.fsm.current;
  }

  onStateChange(fn: (accountId: string, from: SyncState, to: SyncState) => void): void {
    this.fsm.onTransition((from, to) => fn("", from, to));
  }

  /** 注册账户（初始 UNCONNECTED） */
  async register(accountId: string): Promise<void> {
    this.fsm.reset();
    await this.audit(accountId, "account.registered");
  }

  /** 授权 + 启动同步（演示：simulate OAuth 换取并 KMS 加密 token） */
  async authorizeAndStart(accountId: string, accessToken: string): Promise<void> {
    if (!this.fsm.can("AUTH_OK")) this.fsm.reset();
    this.fsm.send("AUTH_OK"); // UNCONNECTED -> AUTHORIZING
    await this.deps.sealer.seal(accessToken); // 模拟换取并 KMS 加密
    await this.audit(accountId, "auth.token_sealed");
    this.fsm.send("AUTH_OK"); // AUTHORIZING -> INITIAL_FULL

    const cred = {
      accountId,
      providerType: this.deps.connector.providerType,
      authMethod: "oauth2" as const,
      accessToken,
    };
    const session = await this.deps.connector.connect(cred);
    this.sessions.set(accountId, session);
    this.stopFlags.delete(accountId);

    await this.runInitialFull(accountId);
    await this.subscribeIncremental(accountId);

    await this.audit(accountId, "sync.started");
  }

  private async runInitialFull(accountId: string): Promise<void> {
    const session = this.sessions.get(accountId)!;
    const cursor = (await this.deps.cursors.get(this.tenantId, accountId, FOLDER)) ?? {};
    const cs = await this.deps.connector.fetchChanges(session, FOLDER, cursor);
    for (const uid of cs.added) {
      await this.publishTask(accountId, uid, cs.nextCursor, 5);
    }
    await this.deps.cursors.put(this.tenantId, accountId, FOLDER, cs.nextCursor);
    this.fsm.send("FULL_DONE"); // INITIAL_FULL -> INCREMENTAL
    await this.audit(accountId, "sync.initial_full_done", `${cs.added.length} uids`);
  }

  private async subscribeIncremental(accountId: string): Promise<void> {
    const session = this.sessions.get(accountId)!;
    if (!this.deps.connector.capabilities.push) return;
    await this.deps.connector.subscribe(session, FOLDER, {
      onExists: async () => {
        if (this.stopFlags.has(accountId)) return;
        try {
          await this.pollChanges(accountId);
        } catch (err) {
          await this.handleError(accountId, err as Error);
        }
      },
    });
  }

  /** 增量轮询（IDLE 回调或定时触发） */
  async pollChanges(accountId: string): Promise<void> {
    if (this.stopFlags.has(accountId)) return;
    const session = this.sessions.get(accountId);
    if (!session) return;
    const cursor = (await this.deps.cursors.get(this.tenantId, accountId, FOLDER)) ?? {};
    const cs = await this.deps.connector.fetchChanges(session, FOLDER, cursor);
    if (cs.added.length > 0) {
      for (const uid of cs.added) {
        await this.publishTask(accountId, uid, cs.nextCursor, 10);
      }
      await this.deps.cursors.put(this.tenantId, accountId, FOLDER, cs.nextCursor);
      this.fsm.send("CHANGE_RECEIVED"); // 自环 INCREMENTAL
    }
  }

  private async handleError(accountId: string, err: Error): Promise<void> {
    const isThrottle = /429|throttl|rate/i.test(err.message);
    if (isThrottle && this.fsm.can("THROTTLED")) {
      this.fsm.send("THROTTLED");
      await this.audit(accountId, "sync.throttled");
      const backoff = Math.min(this.backoffMaxMs, 1000 * (1 + Math.random() * 2));
      setTimeout(async () => {
        if (this.stopFlags.has(accountId)) return;
        this.fsm.send("RESUME"); // THROTTLED -> INCREMENTAL
        await this.pollChanges(accountId);
      }, backoff);
    } else if (this.fsm.can("TRANSIENT_ERR")) {
      this.fsm.send("TRANSIENT_ERR");
    } else if (this.fsm.can("FATAL_ERR")) {
      this.fsm.send("FATAL_ERR");
    }
    await this.audit(accountId, "sync.error", err.message);
  }

  async pause(accountId: string): Promise<void> {
    if (this.fsm.can("PAUSE")) {
      this.fsm.send("PAUSE");
      this.stopFlags.add(accountId);
    }
  }

  async resume(accountId: string): Promise<void> {
    this.stopFlags.delete(accountId);
    if (this.fsm.can("RESUME")) this.fsm.send("RESUME");
    await this.pollChanges(accountId);
  }

  private async publishTask(
    accountId: string,
    uid: string,
    cursor: SyncCursor,
    priority: number,
  ): Promise<void> {
    const ev: SyncTaskEvent = {
      topic: "sync-tasks",
      partitionKey: accountId,
      // 幂等键含租户：防跨租户同 accountId+uid 碰撞
      idempotencyKey: `${this.tenantId}|${accountId}|task|${uid}|${cursor.highestModSeq ?? "0"}`,
      ts: Date.now(),
      tenantId: this.tenantId,
      folder: FOLDER,
      cursor,
      priority,
    };
    await this.deps.bus.publish(ev);
  }

  private async audit(accountId: string, action: string, detail?: string): Promise<void> {
    const ev: AppEvent = {
      topic: "audit",
      partitionKey: accountId,
      idempotencyKey: `audit|${this.tenantId}|${accountId}|${action}|${Date.now()}`,
      ts: Date.now(),
      tenantId: this.tenantId,
      actor: "sync-orchestrator",
      action,
      accountId,
      detail,
    };
    await this.deps.bus.publish(ev);
  }
}
