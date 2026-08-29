/**
 * 连接器注册表（ADR-005：适配器注册表 + 规范邮件模型）。
 * 框架按 providerType 查找并实例化对应连接器；新增协议只需注册实现。
 * 这是"开闭原则"的体现：对扩展开放、对修改封闭。
 */

import type { Connector, ProviderType } from "../model/connector.ts";

export type ConnectorFactory = () => Connector;

export class ConnectorRegistry {
  private factories = new Map<ProviderType, ConnectorFactory>();

  register(provider: ProviderType, factory: ConnectorFactory): void {
    this.factories.set(provider, factory);
  }

  get(provider: ProviderType): Connector {
    const f = this.factories.get(provider);
    if (!f) throw new Error(`no connector registered for provider: ${provider}`);
    return f();
  }

  list(): ProviderType[] {
    return [...this.factories.keys()];
  }
}
