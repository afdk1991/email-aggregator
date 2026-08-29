/**
 * 同步编排状态机（深化文档 §A.2，7 态）。
 * 由 sync-orchestrator 驱动；错误进入退避，成功回到守护态。
 * 采用显式迁移表，非法迁移抛错 —— 让"状态边界"可被测试与审计。
 */

export type SyncState =
  | "UNCONNECTED"
  | "AUTHORIZING"
  | "INITIAL_FULL"
  | "INCREMENTAL"
  | "THROTTLED"
  | "ERROR"
  | "PAUSED";

export type SyncEvent =
  | "AUTH_OK"
  | "FULL_DONE"
  | "CHANGE_RECEIVED"
  | "THROTTLED"
  | "TRANSIENT_ERR"
  | "FATAL_ERR"
  | "RESUME"
  | "PAUSE";

const TRANSITIONS: Record<SyncState, Partial<Record<SyncEvent, SyncState>>> = {
  UNCONNECTED: { AUTH_OK: "AUTHORIZING" },
  AUTHORIZING: { AUTH_OK: "INITIAL_FULL" },
  INITIAL_FULL: { FULL_DONE: "INCREMENTAL", TRANSIENT_ERR: "THROTTLED", FATAL_ERR: "ERROR" },
  INCREMENTAL: {
    CHANGE_RECEIVED: "INCREMENTAL",
    THROTTLED: "THROTTLED",
    TRANSIENT_ERR: "THROTTLED",
    FATAL_ERR: "ERROR",
    PAUSE: "PAUSED",
  },
  THROTTLED: { RESUME: "INCREMENTAL", TRANSIENT_ERR: "ERROR" },
  ERROR: { RESUME: "INCREMENTAL" },
  PAUSED: { RESUME: "INCREMENTAL" },
};

export class SyncStateMachine {
  private state: SyncState = "UNCONNECTED";
  private listeners: Array<(from: SyncState, to: SyncState, ev: SyncEvent) => void> = [];

  get current(): SyncState {
    return this.state;
  }

  onTransition(fn: (from: SyncState, to: SyncState, ev: SyncEvent) => void): void {
    this.listeners.push(fn);
  }

  can(event: SyncEvent): boolean {
    return !!TRANSITIONS[this.state][event];
  }

  /** 触发迁移；返回是否发生状态变化 */
  send(event: SyncEvent): boolean {
    const next = TRANSITIONS[this.state][event];
    if (!next) {
      throw new Error(`illegal transition: ${this.state} --${event}--> ?`);
    }
    if (next === this.state) {
      // 自环（如 INCREMENTAL 收到 CHANGE_RECEIVED），仍通知监听者
      this.listeners.forEach((l) => l(this.state, this.state, event));
      return false;
    }
    const from = this.state;
    this.state = next;
    this.listeners.forEach((l) => l(from, next, event));
    return true;
  }

  reset(): void {
    this.state = "UNCONNECTED";
  }
}
