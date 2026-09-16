#!/usr/bin/env node
/**
 * ============================================================================
 *  移动端产物校验
 * ============================================================================
 *
 *  证明「Android / iOS 安装包确实携带 version.json 里的版本号」：
 *    ① 原生工程版本槽位 = version.json
 *       （android/app/build.gradle 的 versionCode/versionName、ios 的 Info.plist）
 *    ② 产出的 .apk / .aab / .ipa 内容中可检出 MALLVx.y.z
 *       —— 必须走 ZIP 解压查找：这些包都是 ZIP，文本条目默认 deflate 压缩，
 *          直接对文件原文 includes() 会得到**假阴性**。见 zip-search.mjs。
 *    ③ .xcarchive 内 Products/Applications/*.app/Info.plist 的
 *       CFBundleShortVersionString / CFBundleVersion 与期望一致
 *       —— Xcode 会把 Info.plist 重新序列化成二进制 plist，文本正则匹配不到。见 plist.mjs。
 *
 *  用法：node scripts/verify-artifacts.mjs
 *  退出码：0 通过；1 失败（可作为 CI 门禁）
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { readPlistVersion } from './plist.mjs'
import { readVersion } from './prepare.mjs'
import { archiveContains } from './zip-search.mjs'

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

/** 在 .xcarchive 内定位 App 包的 Info.plist；找不到返回 null。 */
function findArchivePlist(xcArchive) {
  const appsDir = path.join(xcArchive, 'Products', 'Applications')
  if (!fs.existsSync(appsDir)) return null
  for (const n of fs.readdirSync(appsDir)) {
    if (!n.endsWith('.app')) continue
    const plist = path.join(appsDir, n, 'Info.plist')
    if (fs.existsSync(plist)) return plist
  }
  return null
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
    const got = readPlistVersion(plist)
    if (got.short === v.semver && got.build === String(v.code)) {
      pass('iOS 版本槽位', `${got.kind}：CFBundleShortVersionString=${got.short}, CFBundleVersion=${got.build}`)
    } else {
      fail('iOS 版本槽位', `${got.kind}：实际 ${got.short} / ${got.build}；期望 ${v.semver} / ${v.code}`)
    }
  } else {
    console.log('[mobile-verify] 未生成 iOS 工程，跳过')
  }

  // ── ② 安装包内容 ───────────────────────────────────────────────────────
  const packages = walk(PKG_DIR).filter((f) => /\.(apk|aab|ipa)$/i.test(f.p))
  if (packages.length === 0) {
    console.log('[mobile-verify] 未发现已构建的 apk/aab/ipa —— 请先执行 npm run build:android / build:ios')
  } else {
    //  必须用 archiveContains 而不是 fs.readFileSync(...).includes()：
    //  APK / AAB / IPA 都是 ZIP，`assets/public/index.html` 里的
    //  <meta name="app-version" content="MALLVx.y.z" /> 默认被 deflate 压缩，
    //  明文版本号在文件原文里通常**不再连续出现**，直接搜原文必得假阴性 ——
    //  表现为"包明明打对了却报未检出版本号"。
    const bad = []
    for (const f of packages) {
      const r = archiveContains(f.p, v.canonical)
      const how = r.how === 'zip' ? `（解压后命中：${r.where.join(', ')}）` : r.how === 'raw' ? '（原文命中）' : ''
      console.log(
        `        ${path.relative(PKG_DIR, f.p)}  ${(f.size / 1024 / 1024).toFixed(1)} MB  ` +
          `版本号${r.hit ? '已检出' : '未检出'}${how}`,
      )
      if (!r.hit) bad.push(path.relative(PKG_DIR, f.p))
    }
    if (bad.length) fail('安装包内含规范版本号', `${bad.length} 个产物未检出 ${v.canonical}：${bad.join(', ')}`)
    else pass('安装包内含规范版本号', `${packages.length} 个产物均含 ${v.canonical}`)
  }

  // ── ③ iOS 归档内的 Info.plist ──────────────────────────────────────────
  const xcRoot = path.join(PKG_DIR, 'ios', 'build')
  const xcArchives = fs.existsSync(xcRoot)
    ? fs
        .readdirSync(xcRoot)
        .filter((n) => n.endsWith('.xcarchive'))
        .map((n) => path.join(xcRoot, n))
    : []
  if (xcArchives.length === 0) {
    console.log('[mobile-verify] 未发现 .xcarchive —— 请先执行 npm run build:ios（或 node scripts/build.mjs ios --unsigned）')
  }
  for (const xc of xcArchives) {
    const label = `iOS 归档版本槽位（${path.relative(PKG_DIR, xc)}）`
    const archivePlist = findArchivePlist(xc)
    if (!archivePlist) {
      fail(label, '归档内未找到 Products/Applications/*.app/Info.plist')
      continue
    }
    const got = readPlistVersion(archivePlist)
    if (got.short == null || got.build == null) {
      fail(label, `${got.kind} 形态的 Info.plist 未能读出 CFBundleShortVersionString / CFBundleVersion`)
    } else if (got.short === v.semver && got.build === String(v.code)) {
      pass(label, `${got.kind}：CFBundleShortVersionString=${got.short}, CFBundleVersion=${got.build}`)
    } else {
      fail(label, `${got.kind}：实际 ${got.short} / ${got.build}；期望 ${v.semver} / ${v.code}`)
    }
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
