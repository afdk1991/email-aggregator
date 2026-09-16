// 全局编译期常量声明（由 vite.config.ts 的 define 注入）。
// 用 global 声明而非模块导出，是为了让各文件里的 __DEMO_MODE__ 都能被
// 直接文本替换，从而在关闭演示模式时让死分支真正被 tree-shaking 掉。
declare const __DEMO_MODE__: boolean
