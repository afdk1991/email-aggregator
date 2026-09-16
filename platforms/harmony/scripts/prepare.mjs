#!/usr/bin/env node
/**
 * ============================================================================
 *  HarmonyOS 资源准备（prepare）
 * ============================================================================
 *
 *  三件事：
 *    1) 版本一致性门禁 —— AppScope/app.json5 的 versionName/versionCode
 *       与 entry 的 AppVersion.ets 必须都等于 version.json（${'`'}MALLVx.y.z`）。
 *    2) 同步前端产物到 entry/src/main/resources/rawfile/www（供 $rawfile 加载）。
 *       复用仓库根的统同一同步脚本，顺带保证另外四处目标也一致。
 *    3) 生成应用图标到 AppScope 与 entry 的 media 目录（DevEco 要求预置资源存在）。
 *
 *  用法：node scripts/prepare.mjs
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath, pathToFileURL } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const PKG_DIR = path.resolve(HERE, '..')
const REPO = path.resolve(PKG_DIR, '..', '..')
const VERSION_TOOL = path.join(REPO, 'scripts', 'version.mjs')
const SYNC_TOOL = path.join(REPO, 'scripts', 'sync-web-assets.mjs')
const ICON_TOOL = path.join(REPO, 'scripts', 'make-icon.mjs')

export function readVersion() {
  const r = spawnSync(process.execPath, [VERSION_TOOL, 'show', '--json'], { encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`无法读取版本：${r.stderr || r.stdout}`)
  return JSON.parse(r.stdout)
}

function assertVersions(v) {
  const issues = []

  const appJson = path.join(PKG_DIR, 'AppScope', 'app.json5')
  if (!fs.existsSync(appJson)) {
    throw new Error(`缺少 ${path.relative(REPO, appJson)}`)
  }
  const txt = fs.readFileSync(appJson, 'utf8')
  const code = txt.match(/versionCode:\s*(\d+)/)?.[1]
  const name = txt.match(/versionName:\s*'([^']*)'/)?.[1]
  if (Number(code) !== v.code) issues.push(`app.json5 versionCode = ${code}，期望 ${v.code}`)
  if (name !== v.canonical) issues.push(`app.json5 versionName = '${name}'，期望 '${v.canonical}'`)

  const ets = path.join(PKG_DIR, 'entry', 'src', 'main', 'ets', 'generated', 'AppVersion.ets')
  if (!fs.existsSync(ets)) {
    issues.push('缺少 entry/src/main/ets/generated/AppVersion.ets')
  } else {
    const et = fs.readFileSync(ets, 'utf8')
    const c = et.match(/APP_VERSION_CANONICAL:\s*string\s*=\s*"([^"]*)"/)?.[1]
    if (c !== v.canonical) issues.push(`AppVersion.ets APP_VERSION_CANONICAL = "${c}"，期望 "${v.canonical}"`)
  }

  if (issues.length) {
    throw new Error(`HarmonyOS 版本号与 version.json（${v.canonical}）不一致：\n  - ${issues.join('\n  - ')}\n请执行：node scripts/version.mjs sync`)
  }
}

function ensureIcons() {
  const appMedia = path.join(PKG_DIR, 'AppScope', 'resources', 'base', 'media')
  const entryMedia = path.join(PKG_DIR, 'entry', 'src', 'main', 'resources', 'base', 'media')
  fs.mkdirSync(appMedia, { recursive: true })
  fs.mkdirSync(entryMedia, { recursive: true })

  const gen = (outDir, png, size) => {
    const args = [ICON_TOOL, '--out', outDir, '--png', png, '--size', String(size), '--no-ico']
    const r = spawnSync(process.execPath, args, { stdio: 'inherit' })
    if (r.status !== 0) throw new Error(`图标生成失败：${png}`)
  }

  if (!fs.existsSync(path.join(appMedia, 'app_icon.png'))) gen(appMedia, 'app_icon.png', 1024)
  if (!fs.existsSync(path.join(entryMedia, 'app_icon.png'))) gen(entryMedia, 'app_icon.png', 512)
  if (!fs.existsSync(path.join(entryMedia, 'startIcon.png'))) gen(entryMedia, 'startIcon.png', 512)
}

export function prepare() {
  const v = readVersion()
  console.log(`[harmony] 版本 ${v.canonical}（semver=${v.semver}, code=${v.code}）`)

  assertVersions(v)
  console.log('[harmony] 版本一致性 OK')

  const r = spawnSync(process.execPath, [SYNC_TOOL], { stdio: 'inherit', cwd: REPO })
  if (r.status !== 0) throw new Error('前端产物同步失败')

  ensureIcons()
  console.log('[harmony] 图标就绪')
  return { version: v }
}

const invokedDirectly =
  process.argv[1] !== undefined && pathToFileURL(process.argv[1]).href === import.meta.url

if (invokedDirectly) {
  try {
    prepare()
    console.log('[harmony] 准备完成')
  } catch (e) {
    console.error(`[harmony] 失败：${e.message}`)
    process.exit(1)
  }
}
