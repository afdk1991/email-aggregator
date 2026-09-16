#!/usr/bin/env node
/**
 * ============================================================================
 *  移动端产物校验
 * ============================================================================
 *
 *  证明「Android / iOS 安装包确实携带 version.json 里的版本号」：
 *    ① 原生工程版本槽位 = version.json（versionCode / versionName / Info.plist）
 *    ② 产出的 .apk / .aab 内容中可检出 MALLVx.y.z
 *    ③ 产出的 .ipa / .xcarchive 内 Info.plist 的 CFBundleShortVersionString 正确
 *
 *  用法：node scripts/verify-artifacts.mjs
 *  退出码：0 通过；1 失败（可作为 CI 门禁）
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { readVersion } from './prepare.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const PKG_DIR = path.resolve(HERE, '..')

const checks = []
const pass = (n, d) => checks.push({ ok: true, n, d })
const fail = (n, d) => checks.push({ ok: false, n, d })

function walk(dir, depth = 0) {
  const out = []
  if (!fs.existsSync(dir) || depth > 8) return out
  for (const n of fs.readdirSync(dir)) {
    if (n === 'node_modules' || n === '.gradle' || n === 'Pods' || n === 'build' && depth === 0) continue
    const p = path.join(dir, n)
    const st = fs.statSync(p)
    if (st.isDirectory()) out.push(...walk(p, depth + 1))
    else out.push({ p, size: st.size })
  }
  return out
}

function main() {
  const v = readVersion()
  console.log(`[mobile-verify] 期望版本 ${v.canonical}（semver=${v.semver}, code=${v.code}）\n`)

  // ── ① 原生工程版本槽位 ─────────────────────────────────────────────────
  const gradle = path.join(PKG_DIR, 'android', 'app', 'build.gradle')
  if (fs.existsSync(gradle)) {
    const t = fs.readFileSync(gradle, 'utf8')
    const code = t.match(/\n\s*versionCode (\d+)/)?.[1]
    const name = t.match(/\n\s*versionName "([^"]*)"/)?.[1]
    if (Number(code) === v.code && name === v.canonical) {
      pass('Android 版本槽位', `versionCode=${code}, versionName="${name}"`)
    } else {
      fail('Android 版本槽位', `实际 versionCode=${code}, versionName="${name}"；期望 ${v.code} / "${v.canonical}"`)
    }
  } else {
    console.log('[mobile-verify] 未生成 Android 工程，跳过')
  }

  const plist = path.join(PKG_DIR, 'ios', 'App', 'App', 'Info.plist')
  if (fs.existsSync(plist)) {
    const t = fs.readFileSync(plist, 'utf8')
    const short = t.match(/<key>CFBundleShortVersionString<\/key>\s*<string>([^<]*)<\/string>/)?.[1]
    const build = t.match(/<key>CFBundleVersion<\/key>\s*<string>([^<]*)<\/string>/)?.[1]
    if (short === v.semver && build === String(v.code)) {
      pass('iOS 版本槽位', `CFBundleShortVersionString=${short}, CFBundleVersion=${build}`)
    } else {
      fail('iOS 版本槽位', `实际 ${short} / ${build}；期望 ${v.semver} / ${v.code}`)
    }
  } else {
    console.log('[mobile-verify] 未生成 iOS 工程，跳过')
  }

  // ── ② 安装包内容 ───────────────────────────────────────────────────────
  const packages = walk(PKG_DIR).filter((f) => /\.(apk|aab|ipa)$/i.test(f.p))
  if (packages.length === 0) {
    console.log('[mobile-verify] 未发现已构建的 apk/aab/ipa —— 请先执行 npm run build:android / build:ios')
  } else {
    const needle = Buffer.from(v.canonical, 'utf8')
    const bad = []
    for (const f of packages) {
      const hit = fs.readFileSync(f.p).includes(needle)
      console.log(`        ${path.relative(PKG_DIR, f.p)}  ${(f.size / 1024 / 1024).toFixed(1)} MB  版本号${hit ? '已检出' : '未检出'}`)
      if (!hit) bad.push(f.p)
    }
    if (bad.length) fail('安装包内含规范版本号', `${bad.length} 个产物未检出 ${v.canonical}`)
    else pass('安装包内含规范版本号', `${packages.length} 个产物均含 ${v.canonical}`)
  }

  console.log('')
  for (const c of checks) console.log(`  ${c.ok ? 'OK  ' : 'FAIL'}  ${c.n}\n        ${c.d}`)
  console.log('')
  const failed = checks.filter((c) => !c.ok)
  if (failed.length) {
    console.log(`[mobile-verify] ${failed.length} 项失败`)
    process.exit(1)
  }
  console.log('[mobile-verify] 全部通过')
}

main()
