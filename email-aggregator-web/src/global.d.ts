// 全局编译期常量声明（由 vite.config.ts 的 define 注入）。
// 用 global 声明而非模块导出，是为了让各文件里的 __DEMO_MODE__ 都能被
// 直接文本替换，从而在关闭演示模式时让死分支真正被 tree-shaking 掉。
declare const __DEMO_MODE__: boolean

// ---------------------------------------------------------------------------
// 环境变量与 import.meta 类型
//
// 本仓库未引用 vite/client 三斜线指令，故在此显式声明，让 tsc --noEmit 能识别
// import.meta.env.*。多端构建依赖 VITE_API_BASE_URL，声明缺失会让类型检查失败。
// ---------------------------------------------------------------------------

interface ImportMetaEnv {
  /** 默认 API 服务端基址。留空 = 与页面同源（Web 直访 / Go 单二进制 / Electron 桌面端）。 */
  readonly VITE_API_BASE_URL?: string
  /** 演示模式开关（'false' 关闭）。由 .env / .env.production 提供。 */
  readonly VITE_DEMO_MODE?: string
  /** 开发代理目标端口（仅 vite dev server 使用）。 */
  readonly VITE_API_PORT?: string
  /** 部署渠道标识，用于前端展示与埋点区分（web / desktop / android / ios / harmony）。 */
  readonly VITE_APP_CHANNEL?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}

