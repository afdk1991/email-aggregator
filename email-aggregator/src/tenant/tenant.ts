/**
 * 多租户上下文与请求层解析（ADR-009 数据面核心骨架）。
 *
 * 设计定位：逻辑多租户共享栈（tenant_id 全链路强制透传）+ 企业版专属栈/私有化（数据不出境），
 * 同一二进制、部署形态参数化（ADR-009）。本模块是数据面（store / search / notify / api）共用的
 * 零依赖基座：定义租户上下文、默认租户、请求头解析，避免各层各自造轮子。
 *
 * 强制透传 vs 最小可逆：tenant_id 以显式参数贯穿 请求 → 存储 → 检索 → 通知，单节点以
 * DefaultTenantID 运行；接口签名保持纯粹「加法」扩展（新增 tenantId 首参），不破坏既有 PoC
 * 行为（单租户下等价于原 accountId 隔离）。后续「完整隔离（多轮）」在此骨架上叠加物理隔离
 * （独立 schema / Citus 分布键切换为 tenant_id）即可，无需重写接口。
 *
 * 与 Go 实现对齐：email-aggregator-go/src/tenant/tenant.go。
 */

import type { IncomingMessage } from "node:http";

/** DefaultTenantID 单节点 / 未携带租户头时的默认租户。所有隔离边界在此租户内收敛。 */
export const DefaultTenantID = "default";

/** HeaderTenantID 请求头键：显式指定租户（通常由前置 L7 网关 / 鉴权层注入）。缺失时回退 DefaultTenantID。 */
export const HeaderTenantID = "x-tenant-id";

/** HeaderTenantTier 请求头键：指定租户类别（private/enterprise/public），可选。 */
export const HeaderTenantTier = "x-tenant-tier";

/** TenantContext 租户上下文：贯穿请求 → 存储 → 检索 → 通知全链路。 */
export interface TenantContext {
  tenantId: string;
  tier: string;
}

/** 便于日志 / 审计观测。 */
export function formatTenantContext(c: TenantContext): string {
  return `tenant=${c.tenantId} tier=${c.tier}`;
}

/**
 * 可解析租户头部的最小请求形状。
 * 兼容 Node 的 IncomingMessage 与任何带 headers 字典的对象，便于在测试与适配层复用。
 */
export interface HeadersLike {
  headers?: Record<string, string | string[] | undefined> | NodeJS.Dict<string | string[]>;
}

/**
 * FromRequest 从 HTTP 请求解析租户上下文。
 * 优先取 X-Tenant-Id；缺省回退 DefaultTenantID（单节点共享栈）。
 * X-Tenant-Tier 可选，缺省为空（下游按默认策略处理）。
 */
export function fromRequest<T extends HeadersLike = IncomingMessage>(req: T): TenantContext {
  const headers = (req.headers ?? {}) as Record<string, string | string[] | undefined>;
  const rawId = headers[HeaderTenantID];
  const id = typeof rawId === "string" ? rawId.trim() : Array.isArray(rawId) ? (rawId[0] ?? "").trim() : "";
  const rawTier = headers[HeaderTenantTier];
  const tier = typeof rawTier === "string" ? rawTier.trim() : Array.isArray(rawTier) ? (rawTier[0] ?? "").trim() : "";
  return { tenantId: id ? id : DefaultTenantID, tier };
}

/** Resolve 规范化租户 ID：空值回退默认租户，避免空字符串击穿隔离边界。 */
export function Resolve(id: string | undefined | null): string {
  if (id === undefined || id === null || id === "") {
    return DefaultTenantID;
  }
  return id;
}
