/**
 * 事件总线。PoC 以 InMemoryEventBus 实现（at-least-once + 幂等去重 + DLQ）。
 * KafkaAdapter 为真实集成 stub：接口一致，落地时替换为 kafkajs 客户端。
 */

import type { AppEvent, Topic } from "./contracts.ts";

export type Handler = (e: AppEvent) => Promise<void> | void;

export interface EventBus {
  publish(event: AppEvent): Promise<void>;
  subscribe(topic: Topic, handler: Handler): void;
  /** 启动消费（InMemory 立即开始派发已注册订阅） */
  start(): Promise<void>;
  stop(): Promise<void>;
}

export class InMemoryEventBus implements EventBus {
  private handlers = new Map<Topic, Handler[]>();
  private seen = new Set<string>(); // idempotencyKey 去重
  private dlq: AppEvent[] = [];
  private running = false;

  subscribe(topic: Topic, handler: Handler): void {
    const list = this.handlers.get(topic) ?? [];
    list.push(handler);
    this.handlers.set(topic, list);
  }

  async publish(event: AppEvent): Promise<void> {
    // 至少一次语义：先记录、再派发；幂等由消费端 seen 集合保证
    if (!this.running) return; // demo 显式 start 后开始派发
    const topic = event.topic;
    const list = this.handlers.get(topic) ?? [];
    for (const h of list) {
      try {
        // 幂等去重
        if (this.seen.has(event.idempotencyKey)) continue;
        this.seen.add(event.idempotencyKey);
        await h(event);
      } catch (err) {
        // 失败进 DLQ，等待人工 / 重试（PoC 仅记录）
        this.dlq.push(event);
        console.error(`[bus] handler failed on ${topic}:`, (err as Error).message);
      }
    }
  }

  async start(): Promise<void> {
    this.running = true;
  }

  async stop(): Promise<void> {
    this.running = false;
  }

  getDlq(): AppEvent[] {
    return this.dlq;
  }
}

/**
 * @deprecated STUB — 未实现。仅作接口文档保留。
 * 真实 Kafka 适配器落地时改为：
 *   import { Kafka } from "kafkajs";
 *   producer.send({ topic, messages: [{ key: partitionKey, value: JSON.stringify(event) }] })
 * 消费端以 consumer-group 实现再均衡（同 accountId 固定分区 → 单账户有序）。
 * Go 版真实实现见 integration/integration.go 中的 KafkaAdapter。
 */
export class KafkaAdapter implements EventBus {
  private brokers: string[];
  constructor(brokers: string[]) {
    this.brokers = brokers;
    console.warn(
      `[KafkaAdapter] stub: 未连接 ${brokers.join(",")}，请接入 kafkajs 实现 publish/subscribe`,
    );
  }
  async publish(_event: AppEvent): Promise<void> {
    throw new Error("KafkaAdapter.publish not implemented");
  }
  subscribe(_topic: Topic, _handler: Handler): void {
    /* noop stub */
  }
  async start(): Promise<void> {}
  async stop(): Promise<void> {}
}
