#!/usr/bin/env node
/**
 * ============================================================================
 *  前端构建产物同步 —— 单一实现，三处目标
 * ============================================================================
 *
 *  本文件取代原先只覆盖 1 个目标的 email-aggregator-go/sync-web.ps1。
 *  改用 Node 的原因有两个，都是被真实事故逼出来的：
 *    1. macOS / Linux 的 CI runner 没有 pwsh，多端构建必须用一个到处都能跑的脚本；
 *    2. 本项目曾两次因「漏同步」导致线上/单二进制跑旧资源，根因是三处目标
 *       各由不同脚本、不同时机维护。收敛为一份实现 + 一次全量比对，才能根治。
 *
 *  五类目标（缺一即视为失败）：
 *    go-webroot      email-aggregator-go/webroot/dist
 *                    —— `//go:build webui` 的 embed 输入。它是构建产物却又是 embed 源，
 *                       过期不会报错，会静默产出「无样式 / 旧版」的单二进制。
 *    edgeone-app     deploy/edgeone-app/{assets,index.html}
 *                    —— EdgeOne Makers 云上静态站点。
 *    desktop-dist    platforms/desktop/resources/app-dist
 *                    —— Electron 桌面端「内置服务未就绪」时的静态兜底页面。
 *    mobile-www      platforms/mobile/www
 *                    —— Capacitor 的 webDir，Android / iOS 原生工程打包它。
 *    harmony-rawfile platforms/harmony/entry/src/main/resources/rawfile/www
 *                    —— HarmonyOS Web 容器以 $rawfile('www/index.html') 加载。
 *
 *  用法：
 *    node scripts/sync-web-assets.mjs            同步（并校验）
 *    node scripts/sync-web-assets.mjs --verify   只校验，不一致 exit 1
 *    node scripts/sync-web-assets.mjs --json     机器可读输出
 *
 *  同步语义：逐文件按相对路径 + SHA-256 比对。目标中「多余的文件」在同步模式下
 *  会被删除 —— 旧版本的 hash 命名 bundle 残留在 embed / 静态站 / 应用包内既是
 *  体积浪费，也让「线上跑的是哪个版本」无法判断。
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import { fileURLToPath, pathToFileURL } from 'node:url'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const SRC = path.join(ROOT, 'email-aggregator-web', 'dist')

/** @typedef {{ id:string, label:string, dir:string, maps?:{from:string,to:string,prune?:boolean}[], mirror?:boolean, optional?:boolean }} Destination */

/** 同步/校验目标。mirror=true 表示整体镜像 dist/ 的内容。 */
export const DESTINATIONS = [
  {
    id: 'go-webroot',
    label: 'Go 单二进制内嵌资源（//go:build webui 的 embed 输入）',
    dir: 'email-aggregator-go/webroot/dist',
    mirror: true,
  },
  {
    id: 'edgeone-app',
    label: 'EdgeOne Makers 云上静态站点',
    dir: 'deploy/edgeone-app',
    optional: true,
    maps: [
      { from: 'assets', to: 'assets', prune: true },
      { from: 'index.html', to: 'index.html' },
    ],
  },
  {
    id: 'desktop-dist',
    label: '桌面端静态兜底资源（Electron extraResources/app-dist）',
    dir: 'platforms/desktop/resources/app-dist',
    mirror: true,
  },
  {
    id: 'mobile-www',
    label: '移动端 WebView 资源（Capacitor webDir → Android/iOS 原生工程）',
    dir: 'platforms/mobile/www',
    mirror: true,
  },
  {
    id: 'harmony-rawfile',
    label: 'HarmonyOS Web 容器资源（$rawfile/www → entry 模块）',
    dir: 'platforms/harmony/entry/src/main/resources/rawfile/www',
    mirror: true,
  },
]

// ============================================================================
//  工具
// ============================================================================

const sha256 = (file) => crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')

function walkFiles(dir, base = dir) {
  const out = []
  if (!fs.existsSync(dir)) return out
  for (const name of fs.readdirSync(dir)) {
    const abs = path.join(dir, name)
    const st = fs.statSync(abs)
    if (st.isDirectory()) out.push(...walkFiles(abs, base))
    else out.push({ rel: path.relative(base, abs).split(path.sep).join('/'), abs, size: st.size })
  }
  return out
}

const rmrf = (p) => fs.rmSync(p, { recursive: true, force: true })

// ============================================================================
//  目标解析
// ============================================================================

/**
 * 把 Destiantion 展开为「相对路径 → 期望来源文件」的映射表。
 * @returns {Map<string,string>} relPath(目标内) → 源文件绝对路径
 */
function planFor(dest) {
  const plan = new Map()
  if (!fs.existsSync(SRC)) return plan

  if (dest.mirror) {
    for (const f of walkFiles(SRC)) plan.set(f.rel, f.abs)
    return plan
  }

  for (const map of dest.maps ?? []) {
    const fromAbs = path.join(SRC, map.from)
    if (!fs.existsSync(fromAbs)) continue
    const st = fs.statSync(fromAbs)
    if (st.isDirectory()) {
      for (const f of walkFiles(fromAbs)) plan.set(`${map.to}/${f.rel}`, f.abs)
    } else {
      plan.set(map.to, fromAbs)
    }
  }
  return plan
}

/** 目标当前实际存在的文件（仅限受管范围，避免误删同目录下的部署脚本等）。 */
function existingFor(dest) {
  const dirAbs = path.join(ROOT, dest.dir)
  const out = new Map()
  if (!fs.existsSync(dirAbs)) return out

  if (dest.mirror) {
    for (const f of walkFiles(dirAbs)) out.set(f.rel, { abs: f.abs, size: f.size })
    return out
  }

  for (const map of dest.maps ?? []) {
    const toAbs = path.join(dirAbs, map.to)
    if (!fs.existsSync(toAbs)) continue
    const st = fs.statSync(toAbs)
    if (st.isDirectory()) {
      for (const f of walkFiles(toAbs)) out.set(`${map.to}/${f.rel}`, { abs: f.abs, size: f.size })
    } else {
      out.set(map.to, { abs: toAbs, size: st.size })
    }
  }
  return out
}

/**
 * 比对单个目标。
 * @returns {{id,label,dir,status:'ok'|'drift'|'skip',missing:string[],diff:string[],extra:string[]}}
 */
export function inspectDestination(dest) {
  const dirAbs = path.join(ROOT, dest.dir)
  const plan = planFor(dest)

  if (!fs.existsSync(dirAbs)) {
    if (dest.optional) return { id: dest.id, label: dest.label, dir: dest.dir, status: 'skip', missing: [], diff: [], extra: [] }
    return {
      id: dest.id,
      label: dest.label,
      dir: dest.dir,
      status: 'drift',
      missing: [...plan.keys()],
      diff: [],
      extra: [],
      note: '目标目录不存在',
    }
  }

  const existing = existingFor(dest)
  const missing = []
  const diff = []

  for (const [rel, srcAbs] of plan) {
    const cur = existing.get(rel)
    if (!cur) missing.push(rel)
    else if (sha256(cur.abs) !== sha256(srcAbs)) diff.push(rel)
  }

  const extra = [...existing.keys()].filter((rel) => !plan.has(rel))

  const status = missing.length || diff.length ? 'drift' : 'ok'
  return { id: dest.id, label: dest.label, dir: dest.dir, status, missing, diff, extra }
}

/** 同步单个目标：删除多余 → 拷贝缺失/不同。全程不整目录删除，避免中途失败把 embed 输入清空。 */
export function syncDestination(dest) {
  const dirAbs = path.join(ROOT, dest.dir)
  if (!fs.existsSync(dirAbs) && dest.optional) {
    return { id: dest.id, action: 'skip', copied: 0, removed: 0 }
  }

  const plan = planFor(dest)
  const existing = existingFor(dest)
  let removed = 0
  let copied = 0

  for (const rel of existing.keys()) {
    if (!plan.has(rel)) {
      fs.rmSync(existing.get(rel).abs, { force: true })
      removed++
    }
  }

  for (const [rel, srcAbs] of plan) {
    const targetAbs = path.join(dirAbs, rel)
    fs.mkdirSync(path.dirname(targetAbs), { recursive: true })
    const cur = existing.get(rel)
    if (cur && sha256(cur.abs) === sha256(srcAbs)) continue
    fs.copyFileSync(srcAbs, targetAbs)
    copied++
  }

  return { id: dest.id, action: 'synced', copied, removed }
}

// ============================================================================
//  CLI
// ============================================================================

function main() {
  const args = process.argv.slice(2)
  const verifyOnly = args.includes('--verify')
  const asJson = args.includes('--json')

  if (!fs.existsSync(SRC)) {
    const msg = `前端构建产物不存在：${SRC}\n请先执行：cd email-aggregator-web && npm run build`
    if (asJson) console.log(JSON.stringify({ ok: false, error: msg }, null, 2))
    else console.error(`[sync-web] ${msg}`)
    process.exit(1)
  }

  if (verifyOnly) {
    const rows = DESTINATIONS.map(inspectDestination)
    const bad = rows.filter((r) => r.status === 'drift')
    if (asJson) {
      console.log(JSON.stringify({ ok: bad.length === 0, source: path.relative(ROOT, SRC), targets: rows }, null, 2))
    } else {
      const srcFiles = walkFiles(SRC).length
      console.log(`[sync-web] 源 ${path.relative(ROOT, SRC)}（${srcFiles} 个文件）`)
      for (const r of rows) {
        const tag = r.status === 'ok' ? 'OK   ' : r.status === 'skip' ? 'SKIP ' : 'DRIFT'
        console.log(`  ${tag}  ${r.dir}`)
        console.log(`         ${r.label}`)
        if (r.note) console.log(`         ${r.note}`)
        for (const f of r.missing.slice(0, 12)) console.log(`         · 缺失 ${f}`)
        if (r.missing.length > 12) console.log(`         · …… 另有 ${r.missing.length - 12} 个缺失`)
        for (const f of r.diff.slice(0, 12)) console.log(`         · 内容不同 ${f}`)
        if (r.diff.length > 12) console.log(`         · …… 另有 ${r.diff.length - 12} 个不同`)
        for (const f of r.extra.slice(0, 12)) console.log(`         · 多余（同步时删除）${f}`)
        if (r.extra.length > 12) console.log(`         · …… 另有 ${r.extra.length - 12} 个多余`)
      }
      console.log(
        bad.length === 0
          ? `[sync-web] ${DESTINATIONS.length} 处产物一致 OK`
          : `[sync-web] ${bad.length} 处不一致 —— 执行 node scripts/sync-web-assets.mjs`,
      )
    }
    if (bad.length) process.exit(1)
    return
  }

  const results = DESTINATIONS.map(syncDestination)
  const rows = DESTINATIONS.map(inspectDestination)
  const bad = rows.filter((r) => r.status === 'drift')

  if (asJson) {
    console.log(JSON.stringify({ ok: bad.length === 0, results, verify: rows }, null, 2))
  } else {
    console.log('[sync-web] 同步前端构建产物')
    for (const r of results) {
      const tag = r.action === 'skip' ? '跳过  ' : '已同步'
      console.log(`  ${tag}  拷贝 ${r.copied}  清理 ${r.removed}   ${DESTINATIONS.find((d) => d.id === r.id).dir}`)
    }
    console.log(
      bad.length === 0
        ? `[sync-web] 同步后复验：${DESTINATIONS.length} 处一致 OK`
        : '[sync-web] 同步后复验仍不一致（请检查权限）',
    )
  }
  if (bad.length) process.exitCode = 1
}

/** 仅在被直接执行时跑 CLI；被其它脚本 import 时只导出函数。 */
const invokedDirectly =
  process.argv[1] !== undefined && pathToFileURL(process.argv[1]).href === import.meta.url

if (invokedDirectly) main()
