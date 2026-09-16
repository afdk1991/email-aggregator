#!/usr/bin/env node
/**
 * ============================================================================
 *  桌面端打包驱动（build）
 * ============================================================================
 *
 *  为什么需要这一层，而不直接 `electron-builder --win`：
 *    安装包文件名要带**规范版本号**（MALLV0.0.1），它来自仓库根 version.json。
 *    electron-builder 的 artifactName 支持 ${env.MALLV_VERSION} 宏，但环境变量
 *    必须由父进程注入 —— npm script 里的 `A && B` 无法跨进程传变量。
 *    所以由本脚本：先 prepare（含版本一致性门禁）→ 注入 env → 再 spawn electron-builder。
 *
 *  用法：
 *    node scripts/build.mjs                 当前平台全量安装包
 *    node scripts/build.mjs --win           Windows（NSIS 安装包 + portable）
 *    node scripts/build.mjs --mac           macOS（dmg + zip，需在 macOS 上执行）
 *    node scripts/build.mjs --linux         Linux（AppImage + deb + rpm）
 *    node scripts/build.mjs --all           三平台（需在对应宿主系统上分别执行）
 *    node scripts/build.mjs --dir           只产出未打包目录（快速验证）
 *    node scripts/build.mjs --skip-sidecar  跳过 Go 编译（仅验证 Electron 配置时）
 * ============================================================================
 */

import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

import { prepare } from './prepare.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const PKG_DIR = path.resolve(HERE, '..')
const requireCjs = createRequire(import.meta.url)

const argv = process.argv.slice(2)
const has = (f) => argv.includes(f)

const wantWin = has('--win')
const wantMac = has('--mac')
const wantLinux = has('--linux')
const wantAll = has('--all')
const wantDir = has('--dir')
const skipSidecar = has('--skip-sidecar')

const HOST_OS = { win32: 'windows', darwin: 'darwin', linux: 'linux' }[process.platform]

/** 解析 electron-builder 的 CLI 入口，避免经 .cmd / npx 走 shell。 */
function resolveElectronBuilderCli() {
  let pkgJson
  try {
    pkgJson = requireCjs.resolve('electron-builder/package.json')
  } catch {
    throw new Error(
      '未找到 electron-builder。请先在 platforms/desktop 下执行：\n    npm install',
    )
  }
  const pkg = requireCjs(pkgJson)
  const bin = typeof pkg.bin === 'string' ? pkg.bin : pkg.bin?.['electron-builder']
  if (!bin) throw new Error('electron-builder 的 package.json 未声明 bin 入口')
  return path.join(path.dirname(pkgJson), bin)
}

function main() {
  let osForPrepare = null
  if (wantWin) osForPrepare = 'windows'
  else if (wantMac) osForPrepare = 'darwin'
  else if (wantLinux) osForPrepare = 'linux'
  else if (!wantAll) osForPrepare = HOST_OS

  if (wantDir && !wantWin && !wantMac && !wantLinux && !wantAll) osForPrepare = HOST_OS

  const { version } = prepare({
    os: wantAll ? null : osForPrepare,
    all: wantAll,
    // --dir 只验证 Electron 配置，不需要真实 sidecar（省一次交叉编译）
    skipSidecar: skipSidecar || wantDir,
  })

  // ---------------------------------------------------------------- 环境变量
  const env = { ...process.env, MALLV_VERSION: version.canonical }

  // electron 二进制与 electron-builder 的 winCodeSign/nsis 等工具包在 GitHub 上，
  // 国内直连常超时。默认走 npmmirror 镜像，可用环境变量覆盖回官方源。
  if (!env.ELECTRON_MIRROR) env.ELECTRON_MIRROR = 'https://npmmirror.com/mirrors/electron/'
  if (!env.ELECTRON_BUILDER_BINARIES_MIRROR) {
    env.ELECTRON_BUILDER_BINARIES_MIRROR = 'https://npmmirror.com/mirrors/electron-builder-binaries/'
  }
  // 未配置签名证书时不要让 electron-builder 去翻钥匙串/证书库
  if (!env.CSC_IDENTITY_AUTO_DISCOVERY) env.CSC_IDENTITY_AUTO_DISCOVERY = 'false'

  const cli = resolveElectronBuilderCli()
  const ebArgs = []
  if (wantAll) ebArgs.push('-mwl')
  if (wantWin) ebArgs.push('--win')
  if (wantMac) ebArgs.push('--mac')
  if (wantLinux) ebArgs.push('--linux')
  if (wantDir) ebArgs.push('--dir')

  console.log(`\n[build] electron-builder ${ebArgs.join(' ') || '(当前平台)'}`)
  console.log(`[build] MALLV_VERSION=${env.MALLV_VERSION}`)
  console.log(`[build] 产物命名 = EmailAggregator-${version.canonical}-<os>-<arch>.<ext>\n`)

  const r = spawnSync(process.execPath, [cli, ...ebArgs], {
    cwd: PKG_DIR,
    env,
    stdio: 'inherit',
  })

  if (r.error) {
    console.error(`[build] 无法启动 electron-builder：${r.error.message}`)
    process.exit(1)
  }
  if (r.status !== 0) {
    console.error(`[build] electron-builder 退出码 ${r.status}`)
    process.exit(r.status ?? 1)
  }

  console.log(`\n[build] 完成。产物目录：platforms/desktop/release`)
  console.log(`[build] 校验版本号是否进入产物：node scripts/verify-artifacts.mjs`)
}

try {
  main()
} catch (e) {
  console.error(`\n[build] 失败：${e.message}`)
  process.exit(1)
}
