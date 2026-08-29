/**
 * 全局配置（PoC）。真实环境应通过配置中心 / 环境变量注入，
 * 敏感项（KMS 端点、DB 连接串）绝不硬编码。
 */

export interface AppConfig {
  env: "demo" | "dev" | "prod";
  httpPort: number;
  wsPort: number;
  // 真实基础设施连接串（demo 模式下不使用，仅作集成参考）
  pgDsn: string;
  minioEndpoint: string;
  opensearchUrl: string;
  kafkaBrokers: string[];
  // 同步限速（每账户每秒 fetch 次数上限）
  fetchRatePerAccount: number;
  // 退避参数
  backoffBaseMs: number;
  backoffMaxMs: number;
  // 长连接占比（用于容量估算）
  idleConnectionRatio: number;
}

export const defaultConfig: AppConfig = {
  env: "demo",
  httpPort: 8080,
  wsPort: 8081,
  pgDsn: "postgres://aggregator:secret@localhost:5432/aggregator",
  minioEndpoint: "http://localhost:9000",
  opensearchUrl: "http://localhost:9200",
  kafkaBrokers: ["localhost:9092"],
  fetchRatePerAccount: 10,
  backoffBaseMs: 1000,
  backoffMaxMs: 60000,
  idleConnectionRatio: 0.6,
};

export function loadConfig(overrides: Partial<AppConfig> = {}): AppConfig {
  return { ...defaultConfig, ...overrides };
}
