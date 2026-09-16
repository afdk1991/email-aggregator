#!/usr/bin/env node
/**
 * ============================================================================
 *  桌面端资源准备（prepare）
 * ============================================================================
 *
 *  打包前把三样东西备齐，缺一都会让安装包「能装但不可用」：
 *
 *    1) resources/bin/       —— 按目标平台交叉编译的 Go 主干单二进制（sidecar）
 *                               `CGO_ENABLED=0 go build -tags webui ./cmd/server`
 *                               产物同时提供 REST + WebSocket + 内嵌 SPA。
 *    2) resources/app-dist/  —— 前端 SPA 构建产物，作为 sidecar 起不来时的静态兜底。
 *    3) build/icon.png|ico   —— 应用图标（若缺失则即时生成）。
 *
 *  同时会先做一次版本一致性前置检查：桌面端 version.generated.cjs 必须与
 *  仓库根 version.json 一致，否则直接失败 —— 避免打出「appId 对、版本号错」的包。
 *
 *  用法：
 *    node scripts/prepare.mjs                      仅当前平台
 *    node scripts/prepare.mjs --os windows         指定平台（windows|darwin|linux）
 *    node scripts/prepare.mjs --all                三平台 × 两架构全量
 *    node scripts/prepare.mjs --skip-sidecar       跳过 Go 编译（开发态快捷）
 *    node scripts/prepare.mjs --skip-web           跳过前端产物同步（已确认一致时）
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { createRequire } from 'node:module'
import { fileURLToPath, pathToFileURL } from 'node:url'

const requireCjs = createRequire(import.meta.url)

const HERE = path.dirname(fileURLToPath(import.meta.url))
const PKG_DIR = path.resolve(HERE, '..')
const REPO = path.resolve(PKG_DIR, '..', '..')
const GO_DIR = path.join(REPO, 'email-aggregator-go')
const VERSION_TOOL = path.join(REPO, 'scripts', 'version.mjs')
const SYNC_TOOL = path.join(REPO, 'scripts', 'sync-web-assets.mjs')
/** 图标生成器为桌面端与 HarmonyOS 共用，故位于仓库根 scripts/。 */
const ICON_TOOL = path.join(REPO, 'scripts', 'make-icon.mjs')
const RES_BIN = path.join(PKG_DIR, 'resources', 'bin')

/** 与 electron-builder.yml 中各平台 target 的 arch 列表保持一致。 */
const ARCH_MATRIX = {
  windows: ['x64', 'arm64'],
  darwin: ['x64', 'arm64'],
  linux: ['x64', 'arm64'],
}

const GOOS = { windows: 'windows', darwin: 'darwin', linux: 'linux' }
const GOARCH = { x64: 'amd64', arm64: 'arm64' }

export const sidecarFileName = (osName, arch) =>
  `email-aggregator-${osName}-${arch === 'x64' ? 'amd64' : arch}${osName === 'windows' ? '.exe' : ''}`

// ============================================================================

function readVersion() {
  const r = spawnSync(process.execPath, [VERSION_TOOL, 'show', '--json'], { encoding: 'utf8' })
  if (r.status !== 0) {
    throw new Error(`无法读取版本（${VERSION_TOOL}）：${r.stderr || r.stdout}`)
  }
  return JSON.parse(r.stdout)
}

/**
 * 版本一致性前置门禁。
 *
 * 只比对「桌面端生成模块」这一处：它是本包唯一被 electron-builder 与主进程读取的
 * 版本来源（package.json 的 version 为 semver 形态，不体现 MALLV 前缀）。
 * 全量落点的校验由 `node scripts/version.mjs verify` 负责，在 CI 里先跑。
 */
function assertVersionInSync(version) {
  const generated = path.join(PKG_DIR, 'src', 'version.generated.cjs')
  if (!fs.existsSync(generated)) {
    throw new Error(
      `缺少 ${path.relative(REPO, generated)}。请先执行：node scripts/version.mjs sync`,
    )
  }
  // 直接用 CJS 载入，而不是「正则剥掉 module.exports 前缀再 JSON.parse」——
  // 生成的 .cjs 是 **JS 对象字面量**（键名不带引号），JSON.parse 必然抛
  // "Expected property name"。更根本的问题是：正则抽取会在生成文件格式微调时
  // 静默算错，而 require() 一定反映真实值。
  const mod = requireCjs(generated)
  if (mod.canonical !== version.canonical || mod.code !== version.code) {
    throw new Error(
      `桌面端版本模块与 version.json 不一致：\n` +
        `  模块内       = ${mod.canonical} (code ${mod.code})\n` +
        `  version.json = ${version.canonical} (code ${version.code})\n` +
        `请执行：node scripts/version.mjs sync`,
    )
  }
  return mod
}

function syncWebAssets() {
  if (!fs.existsSync(SYNC_TOOL)) throw new Error(`缺少共享同步脚本：${SYNC_TOOL}`)
  const r = spawnSync(process.execPath, [SYNC_TOOL], { stdio: 'inherit', cwd: REPO })
  if (r.status !== 0) throw new Error('前端产物同步失败（详见上方输出）')
}

function ensureIcon() {
  const png = path.join(PKG_DIR, 'build', 'icon.png')
  const ico = path.join(PKG_DIR, 'build', 'icon.ico')
  if (fs.existsSync(png) && fs.existsSync(ico)) return
  console.log('[prepare] 图标缺失，即时生成……')
  const r = spawnSync(process.execPath, [ICON_TOOL, '--out', path.join(PKG_DIR, 'build')], {
    stdio: 'inherit',
  })
  if (r.status !== 0) throw new Error('图标生成失败')
}

function buildSidecar(osName, arch, version) {
  const out = path.join(RES_BIN, sidecarFileName(osName, arch))
  fs.mkdirSync(RES_BIN, { recursive: true })

  const env = { ...process.env, CGO_ENABLED: '0', GOOS: GOOS[osName], GOARCH: GOARCH[arch] }
  // 说明：刻意不注入版本 ldflags。version.go 由 version.json 生成即权威；
  // 若用 -X 覆盖 Canonical 而 Code 是 int（-X 无法覆盖非字符串），两者会互相矛盾。
  const args = ['build', '-tags', 'webui', '-ldflags=-s -w', '-o', out, './cmd/server']
  console.log(`[prepare] go build ${GOOS[osName]}/${GOARCH[arch]} → ${path.relative(REPO, out)}`)
  const r = spawnSync('go', args, { cwd: GO_DIR, env, stdio: 'inherit' })
  if (r.error) throw new Error(`无法执行 go（请确认已安装 Go 并在 PATH 中）：${r.error.message}`)
  if (r.status !== 0) throw new Error(`go build 失败：${GOOS[osName]}/${GOARCH[arch]}`)

  const size = fs.statSync(out).size
  // 反证 embed 生效：二进制里必须能搜到版本号字符串（来自 src/version）
  const buf = fs.readFileSync(out)
  const hasVersion = buf.includes(Buffer.from(version.canonical, 'utf8'))
  console.log(
    `[prepare]   ${sidecarFileName(osName, arch)}  ${(size / 1024 / 1024).toFixed(1)} MB  ` +
      `内嵌版本号=${hasVersion ? 'OK' : '未找到'}`,
  )
  return { os: osName, arch, file: out, size, hasVersion }
}

// ============================================================================

export function prepare(opts = {}) {
  const { os = null, all = false, skipSidecar = false, skipWeb = false } = opts
  const version = readVersion()
  console.log(`[prepare] 版本 ${version.canonical} (semver=${version.semver}, code=${version.code})`)
  const mod = assertVersionInSync(version)
  console.log('[prepare] 版本模块一致性 OK')

  if (!skipWeb) syncWebAssets()
  else console.log('[prepare] 跳过前端产物同步（--skip-web）')

  ensureIcon()

  const targets = []
  if (!skipSidecar) {
    if (all) {
      for (const [osName, archs] of Object.entries(ARCH_MATRIX)) {
        for (const arch of archs) targets.push([osName, arch])
      }
    } else {
      const osName = os ?? { win32: 'windows', darwin: 'darwin', linux: 'linux' }[process.platform]
      if (!osName) throw new Error(`未支持的宿主平台 ${process.platform}，请用 --os 指定`)
      for (const arch of ARCH_MATRIX[osName]) targets.push([osName, arch])
    }
    const built = targets.map(([osName, arch]) => buildSidecar(osName, arch, version))
    const missing = built.filter((b) => !b.hasVersion)
    if (missing.length) throw new Error('sidecar 未内嵌版本号，embed 链路可能失效')
  } else {
    console.log('[prepare] 跳过 Go sidecar 编译（--skip-sidecar）')
  }

  return { version, module: mod }
}

// ============================================================================

function parseArgs(argv) {
  return {
    all: argv.includes('--all'),
    skipSidecar: argv.includes('--skip-sidecar'),
    skipWeb: argv.includes('--skip-web'),
    os: argv.includes('--os') ? argv[argv.indexOf('--os') + 1] : null,
  }
}

const invokedDirectly =
  process.argv[1] !== undefined && pathToFileURL(process.argv[1]).href === import.meta.url

if (invokedDirectly) {
  try {
    const { version } = prepare(parseArgs(process.argv.slice(2)))
    console.log(`\n[prepare] 完成 —— ${version.canonical} 已就绪，可执行 electron-builder`)
  } catch (e) {
    console.error(`\n[prepare] 失败：${e.message}`)
    process.exit(1)
  }
}
