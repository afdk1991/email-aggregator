#!/usr/bin/env node
/**
 * ============================================================================
 *  移动端资源准备（prepare）
 * ============================================================================
 *
 *  Capacitor 的原生工程不直接引用前端源码，而是引用 webDir（本包为 `www`）——
 *  一个由 `email-aggregator-web` 构建产物填充的目录。
 *
 *  为避免又出现一处"独立的同步步骤"，这里**复用仓库根的统一同步脚本**
 *  （scripts/sync-web-assets.mjs），它同时保证五处目标一致：
 *    Go webroot · EdgeOne 静态站 · 桌面端静态兜底 · 移动端 www · 鸿蒙 rawfile
 *
 *  同时做版本前置门禁：若原生工程已生成，其 versionName/versionCode 必须与
 *  version.json 一致，否则直接失败 —— 防止打出「版本号对不上」的 APK/IPA。
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

export function readVersion() {
  const r = spawnSync(process.execPath, [VERSION_TOOL, 'show', '--json'], { encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`无法读取版本：${r.stderr || r.stdout}`)
  return JSON.parse(r.stdout)
}

/** 原生工程存在时，校验其版本槽位是否已同步。 */
function assertNativeVersions(v) {
  const issues = []

  const gradle = path.join(PKG_DIR, 'android', 'app', 'build.gradle')
  if (fs.existsSync(gradle)) {
    const txt = fs.readFileSync(gradle, 'utf8')
    const code = txt.match(/\n\s*versionCode (\d+)/)
    const name = txt.match(/\n\s*versionName "([^"]*)"/)
    if (!code || Number(code[1]) !== v.code) {
      issues.push(`android/app/build.gradle versionCode = ${code ? code[1] : '未找到'}，期望 ${v.code}`)
    }
    if (!name || name[1] !== v.canonical) {
      issues.push(`android/app/build.gradle versionName = ${name ? name[1] : '未找到'}，期望 ${v.canonical}`)
    }
  }

  const plist = path.join(PKG_DIR, 'ios', 'App', 'App', 'Info.plist')
  if (fs.existsSync(plist)) {
    const txt = fs.readFileSync(plist, 'utf8')
    const short = txt.match(
      /<key>CFBundleShortVersionString<\/key>\s*<string>([^<]*)<\/string>/,
    )
    const build = txt.match(/<key>CFBundleVersion<\/key>\s*<string>([^<]*)<\/string>/)
    if (!short || short[1] !== v.semver) {
      issues.push(`Info.plist CFBundleShortVersionString = ${short ? short[1] : '未找到'}，期望 ${v.semver}`)
    }
    if (!build || build[1] !== String(v.code)) {
      issues.push(`Info.plist CFBundleVersion = ${build ? build[1] : '未找到'}，期望 ${v.code}`)
    }
  }

  if (issues.length) {
    throw new Error(
      `原生工程版本号与 version.json（${v.canonical}）不一致：\n  - ${issues.join('\n  - ')}\n` +
        `请执行：node scripts/version.mjs sync`,
    )
  }
  return issues.length === 0
}

export function prepare({ quiet = false } = {}) {
  const v = readVersion()
  if (!quiet) console.log(`[mobile] 版本 ${v.canonical}（semver=${v.semver}, code=${v.code}）`)

  const hasAndroid = fs.existsSync(path.join(PKG_DIR, 'android'))
  const hasIOS = fs.existsSync(path.join(PKG_DIR, 'ios'))
  if (hasAndroid || hasIOS) {
    assertNativeVersions(v)
    if (!quiet) console.log('[mobile] 原生工程版本号一致性 OK')
  } else if (!quiet) {
    console.log('[mobile] 尚未生成原生工程，跳过版本校验（先执行 npm run add:android / add:ios）')
  }

  // 统一同步：把前端产物铺到 www（顺带保证其余四处也一致）
  const r = spawnSync(process.execPath, [SYNC_TOOL], { stdio: 'inherit', cwd: REPO })
  if (r.status !== 0) throw new Error('前端产物同步失败')

  return { version: v, hasAndroid, hasIOS }
}

const invokedDirectly =
  process.argv[1] !== undefined && pathToFileURL(process.argv[1]).href === import.meta.url

if (invokedDirectly) {
  try {
    prepare()
    console.log('[mobile] 准备完成')
  } catch (e) {
    console.error(`[mobile] 失败：${e.message}`)
    process.exit(1)
  }
}
