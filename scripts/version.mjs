#!/usr/bin/env node
/**
 * ============================================================================
 *  项目003「邮箱聚合平台」· 多端统一版本号工具链
 * ============================================================================
 *
 *  权威源：仓库根 version.json
 *  规范形态：MALLV<major>.<minor>.<patch>   —— 初始 MALLV0.0.0，后续 MALLV0.0.1 / MALLV0.0.2 逐级递增
 *
 *  三种派生形态（由本工具计算，不在 version.json 中冗余存储）：
 *    canonical  = MALLV0.0.0     用户可见 / 安装包文件名 / 镜像 tag / git tag / 文档标题
 *    semver     = 0.0.0          npm version、iOS CFBundleShortVersionString 等有格式约束处
 *    code       = 0              Android / HarmonyOS versionCode（单调递增整数）
 *
 *  设计约束：
 *    1. 零第三方依赖 —— 只用 node: 内置模块，保证在任意 CI runner 上可直接运行。
 *    2. 不使用 PowerShell —— macOS/Linux runner 无 pwsh，且本项目曾因 .ps1 编码问题
 *       导致部署脚本长期不可运行（详见 scripts/check-ps1-encoding.ps1）。
 *    3. 单向写入：version.json → 所有落点。任何落点都不得反向修改版本。
 *    4. 每个落点槽位独立暴露 render(v)，使「校验」不依赖「写盘」——
 *       早期实现用 write('', v) 反推期望值，对 JSON 型槽位会 JSON.parse('') 抛异常。
 *
 *  用法：
 *    node scripts/version.mjs show                 查看当前版本与派生值
 *    node scripts/version.mjs list                 列出全部落点及其当前/期望值
 *    node scripts/version.mjs verify               校验全部落点一致性（不一致 exit 1）
 *    node scripts/version.mjs sync                 把版本下发到全部落点
 *    node scripts/version.mjs bump patch           递增 patch 并下发（0.0.0 → 0.0.1）
 *    node scripts/version.mjs bump minor           递增 minor 并下发
 *    node scripts/version.mjs bump major           递增 major 并下发
 *    node scripts/version.mjs set MALLV0.0.7       显式设定版本并下发
 *    node scripts/version.mjs artifacts [dir]      在构建产物中检索版本号（证明包内确实带版本）
 *
 *  通用选项：
 *    --json       以 JSON 输出（供 CI / 其他脚本消费）
 *    --dry-run    只显示将要做的改动，不写盘（sync / bump / set 支持）
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const ROOT = path.resolve(HERE, '..')
const VERSION_JSON = path.join(ROOT, 'version.json')

// ============================================================================
//  版本解析与派生
// ============================================================================

const DEFAULT_PREFIX = 'MALLV'
// code 的进位基数：minor/patch 各两位，故 major 上限 9999、minor/patch 上限 99。
// 该上限远高于实际需要，且 MALLV99.99.99 → 999999 远小于 Android 的 2100000000 硬上限。
const MINOR_BASE = 100
const MAJOR_BASE = 10000

function fail(msg) {
  process.stderr.write(`[version] 错误：${msg}\n`)
  process.exit(1)
}

function derive(raw) {
  const prefix = raw.prefix ?? DEFAULT_PREFIX
  const major = Number(raw.major ?? 0)
  const minor = Number(raw.minor ?? 0)
  const patch = Number(raw.patch ?? 0)
  for (const [k, n, val] of [
    ['major', major, raw.major],
    ['minor', minor, raw.minor],
    ['patch', patch, raw.patch],
  ]) {
    if (!Number.isInteger(n) || n < 0) {
      fail(`version.json 的 ${k} 必须是非负整数，实际为 ${JSON.stringify(val)}`)
    }
  }
  if (minor > 99 || patch > 99) {
    fail(`minor/patch 上限为 99（保证 versionCode 单调且不溢出），实际 minor=${minor} patch=${patch}`)
  }
  const semver = `${major}.${minor}.${patch}`
  return {
    prefix,
    major,
    minor,
    patch,
    semver,
    canonical: `${prefix}${semver}`,
    code: major * MAJOR_BASE + minor * MINOR_BASE + patch,
    channel: raw.channel ?? 'dev',
    updatedAt: raw.updatedAt ?? null,
  }
}

function loadVersion() {
  if (!fs.existsSync(VERSION_JSON)) fail(`找不到版本权威源：${VERSION_JSON}`)
  let raw
  try {
    raw = JSON.parse(fs.readFileSync(VERSION_JSON, 'utf8'))
  } catch (e) {
    fail(`version.json 不是合法 JSON：${e.message}`)
  }
  return derive(raw)
}

/** 解析 MALLV0.0.7 或 0.0.7 形式的版本串。 */
function parseVersionString(s) {
  const m = String(s)
    .trim()
    .match(/^(?:([A-Za-z]+))?(\d+)\.(\d+)\.(\d+)$/)
  if (!m) fail(`无法解析版本串 "${s}"（期望形如 MALLV0.0.7 或 0.0.7）`)
  return { prefix: m[1] ?? DEFAULT_PREFIX, major: +m[2], minor: +m[3], patch: +m[4] }
}

/** 归一化空白后再比较，避免缩进/换行差异造成假漂移。 */
const normalize = (s) => String(s).replace(/\s+/g, ' ').trim()

// ============================================================================
//  落点槽位（Edit）工厂
//
//  每个 Edit 暴露三个能力：
//    render(v)         → 该槽位的「期望文本」（校验用，不依赖文件内容）
//    read(text)        → 从文件内容中取出「当前文本」，未找到返回 null
//    write(text, v)    → 返回替换后的完整文件内容
// ============================================================================

function guard(re, what) {
  if (re.global) fail(`内部错误：${what} 的正则不得带 g 标志（会使 test 状态污染）`)
  return re
}

/** JSON 字段槽位：按点分路径读写。 */
function jsonEdit(pointer, label, render) {
  return {
    kind: 'json',
    pointer,
    label,
    render,
    read(text) {
      const val = jsonGet(JSON.parse(text), pointer)
      return val === undefined || val === null ? null : String(val)
    },
    write(text, v) {
      const obj = JSON.parse(text)
      jsonSet(obj, pointer, render(v))
      // package.json 惯例：2 空格缩进 + 末尾换行。保留原文件的末尾换行习惯。
      return JSON.stringify(obj, null, 2) + (text.endsWith('\n') ? '\n' : '')
    },
  }
}

/**
 * 正则槽位：render(v) 返回「整个被替换片段」。
 * 槽位缺失时，可凭 insertAfter 锚点自播种（self-seeding），做到幂等且可重复执行。
 */
function regexEdit(pattern, label, render, opts = {}) {
  guard(pattern, label)
  return {
    kind: 'regex',
    pattern,
    label,
    render,
    read(text) {
      const m = text.match(pattern)
      return m ? m[0] : null
    },
    write(text, v) {
      if (pattern.test(text)) {
        return text.replace(pattern, () => render(v))
      }
      if (opts.insertAfter) {
        const anchor = text.match(guard(opts.insertAfter, `${label}.insertAfter`))
        if (anchor) {
          const at = anchor.index + anchor[0].length
          return text.slice(0, at) + '\n' + render(v) + text.slice(at)
        }
      }
      throw new Error(`槽位定位失败（正则与锚点均未命中）：${label}`)
    },
  }
}

/** 整文件槽位：适用于完全机器所有的文件（如 version.go），最不易漂移。 */
function fileEdit(label, render) {
  return {
    kind: 'file',
    label,
    render,
    read(text) {
      return text
    },
    write(_text, v) {
      return render(v)
    },
  }
}

/** 供 index.html 复用的两个 meta（前端源站与 EdgeOne 静态入口同构）。 */
function htmlVersionMetas() {
  return [
    regexEdit(
      /<meta name="app-version" content="[^"]*"\s*\/>/,
      'app-version meta',
      (v) => `<meta name="app-version" content="${v.canonical}" />`,
      { insertAfter: /<meta name="theme-color"[^>]*\/>/ },
    ),
    regexEdit(
      /<meta name="app-version-code" content="[^"]*"\s*\/>/,
      'app-version-code meta',
      (v) => `<meta name="app-version-code" content="${v.code}" />`,
      { insertAfter: /<meta name="app-version" content="[^"]*"\s*\/>/ },
    ),
  ]
}

/** 供前端与静态页复用的「版本三形态」声明块。 */
function versionTriple(v) {
  return [
    `  canonical: ${JSON.stringify(v.canonical)},`,
    `  semver: ${JSON.stringify(v.semver)},`,
    `  code: ${v.code},`,
  ].join('\n')
}

/**
 * 前端版本模块 —— 整文件生成，由 App 直接 import。
 *
 * 为什么不走 vite define 的 __APP_VERSION__：
 *   define 会把值替换进每一处引用点，产物里散落多份字面量；
 *   而单一模块导出的常量只出现一次，且能被 tsc 静态类型检查、
 *   能被单元测试直接 import 断言。与 __DEMO_MODE__ 的分工明确：
 *   __DEMO_MODE__ 需要在关闭时让死分支被 tree-shaking 掉（必须 define），
 *   版本号则没有这个诉求（必须可被测试直接读取）。
 */
function renderWebVersionTs(v) {
  return `// Code generated by scripts/version.mjs from version.json. DO NOT EDIT.
//
// 修改版本请执行（在仓库根目录）：
//     node scripts/version.mjs bump patch     # MALLV0.0.0 -> MALLV0.0.1
//
// 本文件被提交入库（而非 gitignore），保证全新克隆后 \`npm run build\` 无需
// 先跑版本工具即可通过 —— 避免"构建依赖生成步骤"这类隐式前置条件。

export interface AppVersion {
  /** 规范版本号 MALLV<major>.<minor>.<patch>：用户可见、安装包名、镜像 tag 均用它。 */
  readonly canonical: string
  /** 剥离前缀的 x.y.z 形态，供有格式约束的场合使用。 */
  readonly semver: string
  /** 单调递增整数码 = major*10000 + minor*100 + patch。 */
  readonly code: number
}

export const APP_VERSION: AppVersion = {
${versionTriple(v)}
} as const

export default APP_VERSION
`
}

/**
 * HarmonyOS（ArkTS）版本常量 —— 整文件生成。
 *
 * ArkTS 不能直接 import 前端或 Node 的版本模块，必须有自己的常量文件。
 * 它由 EntryAbility / Index 的 javaScriptProxy 桥暴露给 Web 容器内的页面，
 * 使鸿蒙端页面同样能读出与 version.json 一致的版本号。
 */
function renderHarmonyVersionEts(v) {
  return `// Code generated by scripts/version.mjs from version.json. DO NOT EDIT.
//
// 修改版本请执行（在仓库根目录）：
//     node scripts/version.mjs bump patch     # MALLV0.0.0 -> MALLV0.0.1
//
// ArkTS 侧无法复用前端的 version.generated.ts，故单列一份常量；
// 它通过 Index.ets 的 javaScriptProxy（对象名 eaHarmony）暴露给 Web 容器内的页面。

export const APP_VERSION_CANONICAL: string = ${JSON.stringify(v.canonical)}
export const APP_VERSION_SEMVER: string = ${JSON.stringify(v.semver)}
export const APP_VERSION_CODE: number = ${v.code}
`
}

/**
 * 桌面端（Electron 主进程）版本模块 —— 整文件生成，CommonJS 形态。
 *
 * 为什么是 .cjs 而非 .mjs：Electron 的 preload 在 sandbox 下必须是 CommonJS；
 * 主进程与 preload 共用同一份版本常量时，保持两者同为 CJS 最不易出错。
 * 该文件由 electron-builder 的 files 规则一并打入 asar，运行时 require() 读取，
 * 因此安装包内部也能证明版本号（见 platforms/desktop/scripts/verify-artifacts.mjs）。
 */
function renderDesktopVersionCjs(v) {
  return `// Code generated by scripts/version.mjs from version.json. DO NOT EDIT.
//
// 修改版本请执行（在仓库根目录）：
//     node scripts/version.mjs bump patch     # MALLV0.0.0 -> MALLV0.0.1
//
// 桌面端（Windows / macOS / Linux）的版本号与此处同源，
// 与 Web 端 src/version.generated.ts、Go 端 src/version/version.go、
// 鸿蒙端 entry/src/main/ets/generated/AppVersion.ets 完全一致。

module.exports = {
  canonical: ${JSON.stringify(v.canonical)},
  semver: ${JSON.stringify(v.semver)},
  code: ${v.code},
  prefix: ${JSON.stringify(v.prefix)},
}
`
}

/** version.go —— 整文件生成。用 var 而非 const，便于构建时以 -ldflags -X 覆盖。 */
function renderGoVersion(v) {
  return `// Code generated by scripts/version.mjs from version.json. DO NOT EDIT.
//
// 修改版本请执行（在仓库根目录）：
// node scripts/version.mjs bump patch     # MALLV0.0.0 -> MALLV0.0.1
// 然后提交 version.json 与本文件。
//
// 三个值均为 var 而非 const：允许构建流水线以 -ldflags 在编译期覆盖，
// 例如 -X email-aggregator-go/src/version.Canonical=... ，
// 用于热修复等需要脱离 version.json 的场景。
// 注意本注释刻意不使用缩进块，以保证 gofmt -l 无输出。
package version

// Canonical 是规范版本号 MALLV<major>.<minor>.<patch>：
// 用户可见版本、安装包文件名、容器镜像 tag、git tag、文档标注一律使用它。
var Canonical = ${JSON.stringify(v.canonical)}

// Semver 是剥离前缀的 x.y.z 形态，供有格式约束的场合使用。
var Semver = ${JSON.stringify(v.semver)}

// Code 是单调递增整数码 = major*10000 + minor*100 + patch。
var Code = ${v.code}

// Prefix 是版本前缀，默认 MALLV。
var Prefix = ${JSON.stringify(v.prefix)}
`
}

// ============================================================================
//  落点注册表
// ============================================================================

const TARGETS = [
  // ── 包管理与构建元数据 ────────────────────────────────────────────────
  {
    id: 'web-pkg',
    file: 'email-aggregator-web/package.json',
    label: '前端 SPA 包元数据',
    edits: [jsonEdit('version', 'version（semver）', (v) => v.semver)],
  },
  {
    id: 'poc-pkg',
    file: 'email-aggregator/package.json',
    label: 'TS PoC 包元数据',
    edits: [jsonEdit('version', 'version（semver）', (v) => v.semver)],
  },
  {
    id: 'desktop-pkg',
    file: 'platforms/desktop/package.json',
    label: '桌面端外壳包元数据',
    edits: [jsonEdit('version', 'version（semver）', (v) => v.semver)],
  },
  {
    id: 'mobile-pkg',
    file: 'platforms/mobile/package.json',
    label: '移动端外壳包元数据',
    edits: [jsonEdit('version', 'version（semver）', (v) => v.semver)],
  },
  {
    id: 'root-version-file',
    file: 'VERSION',
    label: '仓库根 VERSION 纯文本',
    edits: [fileEdit('VERSION', (v) => `${v.canonical}\n`)],
  },

  // ── Go 主干 ───────────────────────────────────────────────────────────
  {
    id: 'go-version',
    file: 'email-aggregator-go/src/version/version.go',
    label: 'Go 版本包（/api/health 数据源）',
    edits: [fileEdit('version.go（整文件生成）', renderGoVersion)],
  },

  // ── 前端与线上静态入口 ────────────────────────────────────────────────
  {
    id: 'web-index-html',
    file: 'email-aggregator-web/index.html',
    label: 'SPA index.html 版本元信息',
    edits: htmlVersionMetas(),
  },
  {
    id: 'edgeone-index-html',
    file: 'deploy/edgeone-app/index.html',
    label: 'EdgeOne 静态入口 index.html',
    optional: true,
    reason: '构建产物，由前端同步流程覆盖；本工具负责保持其版本槽位正确',
    edits: htmlVersionMetas(),
  },

  // ── 服务端 / 云函数 ───────────────────────────────────────────────────
  {
    id: 'edgeone-cf',
    file: 'deploy/edgeone-app/cloud-functions/api/[[default]].js',
    label: 'EdgeOne 云函数（serverless-blob 形态）',
    edits: [
      regexEdit(
        /const APP_VERSION = '[^']*'/,
        'APP_VERSION',
        (v) => `const APP_VERSION = '${v.canonical}'`,
        { insertAfter: /^const DEFAULT_ACCOUNT = '[^']*';?$/m },
      ),
      regexEdit(
        /const APP_VERSION_CODE = \d+/,
        'APP_VERSION_CODE',
        (v) => `const APP_VERSION_CODE = ${v.code}`,
        { insertAfter: /^const APP_VERSION = '[^']*';?$/m },
      ),
    ],
  },

  // ── EdgeOne Makers 部署工作区元数据 ───────────────────────────────────
  // 这两处是「部署形态」的包元数据，CLI 打包与平台构建时读它。
  // 它们与业务代码无关，极易被漏掉，导致「项目版本号 MALLV0.0.0 / 部署工作区 1.0.0」并存。
  {
    id: 'edgeone-pkg',
    file: 'deploy/edgeone-app/package.json',
    label: 'EdgeOne 部署工作区包元数据',
    edits: [jsonEdit('version', 'version（semver）', (v) => v.semver)],
  },
  {
    id: 'edgeone-lock',
    file: 'deploy/edgeone-app/package-lock.json',
    label: 'EdgeOne 部署工作区锁文件版本',
    edits: [
      jsonEdit('version', 'version（semver）', (v) => v.semver),
      // lockfile v3 的「根包」挂在 packages 对象下、键名为空串，
      // 点分路径因此写作 'packages..version'（jsonGet 会正确地取 packages['']）。
      jsonEdit('packages..version', 'packages[""].version（semver）', (v) => v.semver),
    ],
  },

  // ── 容器与编排 ────────────────────────────────────────────────────────
  {
    id: 'docker-compose-tag',
    file: 'deploy/container/docker-compose.yml',
    label: '容器镜像 tag',
    edits: [
      regexEdit(
        /image: email-aggregator:p003[0-9A-Za-z.\-]*/,
        'image tag',
        (v) => `image: email-aggregator:p003-${v.canonical}`,
      ),
    ],
  },
  {
    id: 'dockerfile-labels',
    file: 'deploy/container/Dockerfile',
    label: '容器镜像 OCI 标签',
    edits: [
      regexEdit(
        /LABEL org\.opencontainers\.image\.version="[^"]*"/,
        'OCI version 标签',
        (v) => `LABEL org.opencontainers.image.version="${v.canonical}"`,
        { insertAfter: /^FROM .*$/m },
      ),
      regexEdit(
        /LABEL org\.opencontainers\.image\.revision="[^"]*"/,
        'OCI revision 标签',
        (v) => `LABEL org.opencontainers.image.revision="${v.canonical}"`,
        { insertAfter: /LABEL org\.opencontainers\.image\.version="[^"]*"/ },
      ),
    ],
  },
  {
    id: 'web-dockerfile-labels',
    file: 'email-aggregator-web/Dockerfile',
    label: '前端镜像 OCI 标签',
    edits: [
      regexEdit(
        /LABEL org\.opencontainers\.image\.version="[^"]*"/,
        'OCI version 标签',
        (v) => `LABEL org.opencontainers.image.version="${v.canonical}"`,
        { insertAfter: /^FROM .*$/m },
      ),
    ],
  },

  // ── 桌面端（Electron）产物 ────────────────────────────────────────────
  {
    id: 'electron-builder',
    file: 'platforms/desktop/electron-builder.yml',
    label: '桌面端安装包命名规则',
    edits: [
      // 必须匹配**顶格**键（正则不加 \s*）：dmg 段内也有一行缩进的 artifactName，
      // 若允许前导空白，这里会命中 dmg 的那行，把顶层键又改成 dmg 的子键。
      regexEdit(
        /^artifactName: .*$/m,
        'artifactName',
        () => 'artifactName: ${productName}-${env.MALLV_VERSION}-${os}-${arch}.${ext}',
      ),
    ],
  },

  // ── 移动端原生工程（脚手架生成后由本工具接管）─────────────────────────
  {
    id: 'android-gradle',
    file: 'platforms/mobile/android/app/build.gradle',
    label: 'Android build.gradle',
    optional: true,
    reason: '需先执行 `npx cap add android` 生成原生工程',
    edits: [
      regexEdit(/\n\s*versionCode \d+/, 'versionCode', (v) => `\n        versionCode ${v.code}`),
      regexEdit(/\n\s*versionName "[^"]*"/, 'versionName', (v) => `\n        versionName "${v.canonical}"`),
    ],
  },
  {
    id: 'ios-plist',
    file: 'platforms/mobile/ios/App/App/Info.plist',
    label: 'iOS Info.plist',
    optional: true,
    reason: '需先执行 `npx cap add ios` 生成原生工程（且需 macOS + Xcode 才能产出 .ipa）',
    edits: [
      regexEdit(
        /<key>CFBundleShortVersionString<\/key>\s*<string>[^<]*<\/string>/,
        'CFBundleShortVersionString',
        (v) => `<key>CFBundleShortVersionString</key>\n\t<string>${v.semver}</string>`,
      ),
      regexEdit(
        /<key>CFBundleVersion<\/key>\s*<string>[^<]*<\/string>/,
        'CFBundleVersion',
        (v) => `<key>CFBundleVersion</key>\n\t<string>${v.code}</string>`,
      ),
    ],
  },

  // ── HarmonyOS 工程 ────────────────────────────────────────────────────
  {
    id: 'harmony-app-json5',
    file: 'platforms/harmony/AppScope/app.json5',
    label: '鸿蒙应用清单（versionCode / versionName，DevEco 打包读取）',
    edits: [
      // app.json5 是 JSON5，本工程的键名带引号，故正则必须写成 "versionCode":\s*…
      // 若用裸 `versionCode: \d+` 会漏匹配 —— 这条落点曾因此整条丢失，
      // 表现为 bump 后鸿蒙版本号静默不更新，直到 platforms/harmony/scripts/prepare.mjs
      // 的一致性断言在出包时才失败。
      regexEdit(/"versionCode":\s*\d+/, 'versionCode', (v) => `"versionCode": ${v.code}`),
      regexEdit(/"versionName":\s*'[^']*'/, 'versionName', (v) => `"versionName": '${v.canonical}'`),
    ],
  },

  // ── 各端版本常量模块（整文件生成，供运行时读取，是"版本可追踪"的落点）────
  {
    id: 'web-version-ts',
    file: 'email-aggregator-web/src/version.generated.ts',
    label: '前端版本模块（Web/桌面/移动端页面均 import 它）',
    edits: [fileEdit('version.generated.ts（整文件生成）', renderWebVersionTs)],
  },
  {
    id: 'desktop-version-cjs',
    file: 'platforms/desktop/src/version.generated.cjs',
    label: '桌面端版本模块（Electron 主进程 / preload）',
    edits: [fileEdit('version.generated.cjs（整文件生成）', renderDesktopVersionCjs)],
  },
  {
    id: 'harmony-version-ets',
    file: 'platforms/harmony/entry/src/main/ets/generated/AppVersion.ets',
    label: '鸿蒙端版本常量（经 javaScriptProxy 暴露给 Web 容器）',
    edits: [fileEdit('AppVersion.ets（整文件生成）', renderHarmonyVersionEts)],
  },
]

// ============================================================================
//  JSON 点分路径读写
// ============================================================================

function jsonGet(obj, pointer) {
  let cur = obj
  for (const part of pointer.split('.')) {
    if (cur === null || typeof cur !== 'object') return undefined
    cur = cur[part]
  }
  return cur
}

function jsonSet(obj, pointer, value) {
  const parts = pointer.split('.')
  let cur = obj
  for (let i = 0; i < parts.length - 1; i++) cur = cur[parts[i]]
  cur[parts[parts.length - 1]] = value
}

// ============================================================================
//  落点定义自检
//
//  只有落入 README 的第 2 条约束（校验不依赖写盘）才安全；此处做结构性断言。
// ============================================================================

function selfCheck() {
  const problems = []
  const seen = new Set()
  for (const t of TARGETS) {
    if (seen.has(t.id)) problems.push(`落点 id 重复：${t.id}`)
    seen.add(t.id)
    if (!Array.isArray(t.edits) || t.edits.length === 0) problems.push(`落点 ${t.id} 没有任何 edit`)
    for (const e of t.edits ?? []) {
      if (typeof e.render !== 'function') problems.push(`落点 ${t.id}/${e.label} 缺少 render(v)`)
      if (typeof e.read !== 'function') problems.push(`落点 ${t.id}/${e.label} 缺少 read(text)`)
      if (typeof e.write !== 'function') problems.push(`落点 ${t.id}/${e.label} 缺少 write(text, v)`)
    }
  }
  return problems
}

// ============================================================================
//  落点扫描与写入
// ============================================================================

const RANK = { ok: 0, skip: 0, drift: 1, missing: 2, error: 3 }

/**
 * 扫描单个落点。
 * @returns {{id,file,label,status:'ok'|'drift'|'missing'|'skip'|'error',detail,edits:Array}}
 */
/**
 * 整文件生成的落点（kind 全为 file）允许「不存在 → 由 sync 创建」，
 * 例如仓库根 VERSION 与 Go 的 version.go（后者是 Code generated 文件，不应手工维护）。
 * 其它形态的落点必须先由人/脚手架播种槽位，不能凭空创建 —— 否则会把
 * 一个空文件写成「合法但不完整」的配置，静默破坏构建。
 */
const isCreatable = (target) => target.edits.every((e) => e.kind === 'file')

function inspect(target, v) {
  const abs = path.join(ROOT, target.file)
  const base = { id: target.id, file: target.file, label: target.label }

  if (!fs.existsSync(abs)) {
    if (target.optional) {
      return { ...base, status: 'skip', detail: `文件不存在 —— ${target.reason ?? '可选落点'}`, edits: [] }
    }
    if (isCreatable(target)) {
      return {
        ...base,
        status: 'missing',
        detail: '文件不存在，将由 sync 生成',
        edits: target.edits.map((e) => ({
          label: e.label,
          status: 'missing',
          current: null,
          expected: e.render(v),
          detail: '',
        })),
      }
    }
    return { ...base, status: 'missing', detail: '文件不存在，无法校验', edits: [] }
  }

  let text
  try {
    text = fs.readFileSync(abs, 'utf8')
  } catch (e) {
    return { ...base, status: 'error', detail: `读取失败：${e.message}`, edits: [] }
  }

  const edits = []
  let worst = 'ok'

  for (const edit of target.edits) {
    let current = null
    let expected = null
    let detail = ''
    let status

    try {
      expected = edit.render(v)
      current = edit.read(text)
    } catch (e) {
      status = 'error'
      detail = e.message
    }

    if (status === undefined) {
      if (current === null) status = 'missing'
      else if (normalize(current) === normalize(expected)) status = 'ok'
      else status = 'drift'
    }

    edits.push({ label: edit.label, status, current, expected, detail })
    if (RANK[status] > RANK[worst]) worst = status
  }

  return { ...base, status: worst, detail: '', edits }
}

/** 对单个落点执行写入（幂等；无变化则不触碰磁盘）。 */
function applyTarget(target, v) {
  const abs = path.join(ROOT, target.file)
  if (!fs.existsSync(abs)) {
    if (target.optional) return { id: target.id, action: 'skip', file: target.file }
    // 整文件生成的落点（VERSION / version.go / *.generated.*）允许由 sync 创建；
    // 其它形态必须先由人/脚手架播种槽位 —— 否则会把一个空文件写成
    // 「合法但不完整」的配置，静默破坏构建。
    if (!isCreatable(target)) throw new Error(`落点文件不存在：${target.file}`)
    fs.mkdirSync(path.dirname(abs), { recursive: true })
    let created = ''
    for (const edit of target.edits) created = edit.write(created, v)
    fs.writeFileSync(abs, created, 'utf8')
    return { id: target.id, action: 'created', file: target.file }
  }
  const before = fs.readFileSync(abs, 'utf8')
  let after = before
  for (const edit of target.edits) after = edit.write(after, v)
  if (after === before) return { id: target.id, action: 'unchanged', file: target.file }
  fs.writeFileSync(abs, after, 'utf8')
  return { id: target.id, action: 'written', file: target.file }
}

// ============================================================================
//  命令实现
// ============================================================================

const argv = process.argv.slice(2)
const flags = new Set(argv.filter((a) => a.startsWith('--')))
const positional = argv.filter((a) => !a.startsWith('--'))
const cmd = positional[0] ?? 'show'
const JSON_OUT = flags.has('--json')
const DRY = flags.has('--dry-run')

const out = (s = '') => process.stdout.write(s + '\n')

const summarize = (v) => ({
  canonical: v.canonical,
  semver: v.semver,
  code: v.code,
  prefix: v.prefix,
  channel: v.channel,
  updatedAt: v.updatedAt,
})

function cmdShow(v) {
  if (JSON_OUT) return out(JSON.stringify(summarize(v), null, 2))
  out('项目003「邮箱聚合平台」版本号')
  out('─'.repeat(52))
  out(`  规范版本号  canonical   ${v.canonical}`)
  out(`  平台合规号  semver      ${v.semver}`)
  out(`  单调整数码  code        ${v.code}`)
  out(`  渠道        channel     ${v.channel}`)
  out(`  最后更新    updatedAt   ${v.updatedAt ?? '—'}`)
  out(`  权威源                  version.json`)
}

function cmdList(v) {
  const rows = TARGETS.map((t) => inspect(t, v))
  if (JSON_OUT) return out(JSON.stringify({ version: summarize(v), targets: rows }, null, 2))
  out(`期望版本：${v.canonical}   (semver=${v.semver}, code=${v.code})`)
  out('')
  const glyph = { ok: 'OK   ', drift: 'DRIFT', missing: '缺失 ', skip: 'SKIP ', error: 'ERR  ' }
  for (const r of rows) {
    out(`${glyph[r.status]}  ${r.file}`)
    out(`          ${r.label}`)
    for (const e of r.edits) {
      if (e.status === 'ok') out(`          · ${e.label} = ${e.current}`)
      else if (e.status === 'missing')
        out(`          · ${e.label}  未找到槽位  期望=${JSON.stringify(e.expected)}`)
      else if (e.status === 'error') out(`          · ${e.label}  读取错误：${e.detail}`)
      else
        out(
          `          · ${e.label}\n            当前=${JSON.stringify(e.current)}\n            期望=${JSON.stringify(e.expected)}`,
        )
    }
    if (r.detail) out(`          ${r.detail}`)
  }
}

function cmdVerify(v) {
  const rows = TARGETS.map((t) => inspect(t, v))
  const bad = rows.filter((r) => ['drift', 'missing', 'error'].includes(r.status))
  const skipped = rows.filter((r) => r.status === 'skip')

  if (JSON_OUT) {
    out(JSON.stringify({ version: summarize(v), ok: bad.length === 0, bad, skipped }, null, 2))
  } else {
    out(`[version] 校验 ${rows.length} 个落点，期望 ${v.canonical}`)
    for (const r of skipped) out(`  SKIP   ${r.file}\n         ${r.detail}`)
    for (const r of bad) {
      out(`  FAIL   ${r.file}   (${r.label})`)
      for (const e of r.edits) {
        if (e.status === 'ok') continue
        out(`         · ${e.label}`)
        out(`           当前 = ${JSON.stringify(e.current)}`)
        out(`           期望 = ${JSON.stringify(e.expected)}`)
        if (e.detail) out(`           说明 = ${e.detail}`)
      }
    }
  }

  if (bad.length > 0) {
    if (!JSON_OUT) {
      out('')
      out(`[version] 不一致 ${bad.length} 处 —— 执行下方命令修复：`)
      out('    node scripts/version.mjs sync')
    }
    process.exit(1)
  }
  if (!JSON_OUT) out(`[version] 全部一致 OK（跳过 ${skipped.length} 个未生成脚手架的落点）`)
}

function cmdSync(v) {
  const results = TARGETS.map((t) => {
    try {
      return DRY ? { id: t.id, action: 'dry-run', file: t.file } : applyTarget(t, v)
    } catch (e) {
      return { id: t.id, action: 'error', file: t.file, error: e.message }
    }
  })
  const errors = results.filter((r) => r.action === 'error')

  if (JSON_OUT) {
    out(JSON.stringify({ version: summarize(v), dryRun: DRY, results }, null, 2))
  } else {
    out(`[version] ${DRY ? '（dry-run）' : ''}下发 ${v.canonical} 到 ${TARGETS.length} 个落点`)
    for (const r of results) {
      const tag =
        { written: '写入  ', created: '新建  ', unchanged: '已一致', skip: '跳过  ', error: '失败  ', 'dry-run': '待检查' }[
          r.action
        ] ?? r.action
      out(`  ${tag}  ${r.file}${r.error ? '  —— ' + r.error : ''}`)
    }
  }

  if (errors.length) {
    process.exitCode = 1
    if (!JSON_OUT) out(`\n[version] ${errors.length} 个落点失败。可选落点请先按文档生成脚手架后再同步。`)
  }
}

function writeVersionJson(v) {
  const raw = JSON.parse(fs.readFileSync(VERSION_JSON, 'utf8'))
  raw.prefix = v.prefix
  raw.major = v.major
  raw.minor = v.minor
  raw.patch = v.patch
  raw.channel = v.channel
  raw.updatedAt = new Date().toISOString().slice(0, 10)
  fs.writeFileSync(VERSION_JSON, JSON.stringify(raw, null, 2) + '\n', 'utf8')
}

function cmdBump(v, part) {
  if (!['major', 'minor', 'patch'].includes(part)) {
    fail(`bump 需要参数 major|minor|patch，实际 "${part ?? ''}"`)
  }
  const next = { ...v }
  if (part === 'major') {
    next.major += 1
    next.minor = 0
    next.patch = 0
  } else if (part === 'minor') {
    next.minor += 1
    next.patch = 0
  } else {
    next.patch += 1
  }
  return cmdSet(next)
}

function cmdSet(next) {
  const derived = derive(next)
  out(`[version] 目标版本 → ${derived.canonical}  (semver=${derived.semver}, code=${derived.code})`)
  if (DRY) {
    out('[version] dry-run：version.json 未写盘。')
    return cmdSync(derived)
  }
  writeVersionJson(derived)
  out(`[version] 已更新 version.json`)
  cmdSync(derived)
  out('')
  out(`[version] 建议的发布动作：`)
  out(`    git add -A && git commit -m "release: ${derived.canonical}"`)
  out(`    git tag -a ${derived.canonical} -m "邮箱聚合平台 ${derived.canonical}"`)
  out(`    git push origin master --tags      # 触发多平台构建工作流`)
}

/**
 * 在构建产物中检索规范版本号，用于证明「打出来的包确实带上了版本」。
 * 默认检索桌面端与移动端的发布产物目录。
 */
function cmdArtifacts(v) {
  const dir = positional[1] ? path.resolve(ROOT, positional[1]) : null
  const roots = dir
    ? [dir]
    : [
        path.join(ROOT, 'platforms/desktop/release'),
        path.join(ROOT, 'platforms/mobile/dist'),
        path.join(ROOT, 'platforms/harmony/build'),
        path.join(ROOT, 'email-aggregator-go/release'),
      ]

  const needle = v.canonical
  const hits = []
  const scanned = []

  const walk = (p) => {
    if (!fs.existsSync(p)) return
    const st = fs.statSync(p)
    if (st.isDirectory()) {
      for (const name of fs.readdirSync(p)) walk(path.join(p, name))
      return
    }
    if (st.size > 400 * 1024 * 1024) return
    scanned.push(path.relative(ROOT, p))
    // 文件名命中即算 —— 安装包命名规则本身带版本号
    if (path.basename(p).includes(needle)) {
      hits.push({ file: path.relative(ROOT, p), how: '文件名' })
      return
    }
    if (st.size <= 64 * 1024 * 1024) {
      try {
        const buf = fs.readFileSync(p)
        if (buf.includes(Buffer.from(needle, 'utf8'))) hits.push({ file: path.relative(ROOT, p), how: '内容' })
      } catch {
        /* 不可读则忽略 */
      }
    }
  }

  for (const r of roots) walk(r)

  if (JSON_OUT) out(JSON.stringify({ needle, scanned: scanned.length, hits }, null, 2))
  else {
    out(`[version] 在产物中检索 ${needle}，已扫描 ${scanned.length} 个文件`)
    if (hits.length === 0) {
      out('  未命中。若产物尚未构建，请先执行对应的构建命令。')
      process.exitCode = 1
    } else {
      for (const h of hits) out(`  命中[${h.how}]  ${h.file}`)
    }
  }
}

// ============================================================================
//  入口
// ============================================================================

function main() {
  const problems = selfCheck()
  if (problems.length) {
    for (const p of problems) process.stderr.write(`[version] 内部定义错误：${p}\n`)
    process.exit(2)
  }

  const v = loadVersion()

  switch (cmd) {
    case 'show':
      return cmdShow(v)
    case 'list':
      return cmdList(v)
    case 'verify':
    case 'check':
      return cmdVerify(v)
    case 'sync':
      return cmdSync(v)
    case 'bump':
      return cmdBump(v, positional[1])
    case 'set':
      if (!positional[1]) fail('set 需要版本串参数，例如 set MALLV0.0.7')
      return cmdSet(parseVersionString(positional[1]))
    case 'artifacts':
      return cmdArtifacts(v)
    default:
      return fail(`未知命令 "${cmd}"。可用：show | list | verify | sync | bump <part> | set <ver> | artifacts [dir]`)
  }
}

main()
