/**
 * 可选 peerDep 的最小类型声明（对齐 Go `-tags integration` 的语义）。
 *
 * 这些包（pg / minio / kafkajs）在 PoC 默认零依赖运行时不安装；
 * 真实集成时通过 `npm i pg minio kafkajs` 安装，运行时由 integration.ts
 * 中的动态 import 加载。本声明文件让 TypeScript 类型检查在没有这些包
 * 安装的情况下也能通过（相当于 Go 的 build tag 保护）。
 *
 * 不声明具体类型：integration.ts 内部用本地最小接口（PgPoolLike / MinioClientLike /
 * KafkaLike 等）做形态约束，避免与真实包类型耦合。
 */

declare module "pg" {
  const Pool: { new (config: unknown): unknown };
  export { Pool };
  // 兼容 default export 形态
  const _default: { Pool: typeof Pool };
  export default _default;
}

declare module "minio" {
  const Client: { new (config: unknown): unknown };
  export { Client };
  const _default: { Client: typeof Client };
  export default _default;
}

declare module "kafkajs" {
  export class Kafka {
    constructor(config: unknown);
    producer(): unknown;
    consumer(opts: unknown): unknown;
  }
}
