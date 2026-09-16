#!/usr/bin/env node
/**
 * ============================================================================
 *  HarmonyOS 出包（build）
 * ============================================================================
 *
 *  流程：版本门禁 + 同步前端产物 + 生成图标 → 调用 DevEco 的 hvigorw 构建 HAP。
 *
 *  环境前置（不满足时本脚本明确报错，不会静默产出空包）：
 *    · DevEco Studio 5.0 及以上（提供 hvigor、ohpm、Node 运行时）
 *    · HarmonyOS SDK（API 12 / 5.0.0）
 *    · 签名：AppScope/build-profile.json5 的 signingConfigs 需填入你自己的签名配置，
 *            否则只能产出未签名 HAP，无法安装到真机。
 *    · 环境变量 DEVECO_SDK_HOME 指向 SDK 根目录；hvigorw 需在 PATH 或指定 --hvigor 路径。
 *
 *  用法：
 *    node scripts/build.mjs                Release HAP
 *    node scripts/build.mjs --mode debug   Debug HAP
 *    node scripts/build.mjs --hvigor <path-to-hvigorw>
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { prepare } from './prepare.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const PKG_DIR = path.resolve(HERE, '..')
const REPO = path.resolve(PKG_DIR, '..', '..')

const argv = process.argv.slice(2)
const mode = argv.includes('--mode') ? argv[argv.indexOf('--mode') + 1] : 'release'
const hvigorArg = argv.includes('--hvigor') ? argv[argv.indexOf('--hvigor') + 1] : null

function findHvigor() {
  if (hvigorArg) return hvigorArg
  const candidates = [
    path.join(PKG_DIR, 'hvigorw'),
    path.join(PKG_DIR, 'hvigorw.bat'),
    process.env.DEVECO_HVIGORW ?? '',
  ].filter(Boolean)
  for (const c of candidates) if (fs.existsSync(c)) return c
  return process.platform === 'win32' ? 'hvigorw.bat' : 'hvigorw'
}

function main() {
  const { version } = prepare()

  if (!process.env.DEVECO_SDK_HOME && !process.env.HOS_SDK_HOME) {
    throw new Error(
      '未检测到 HarmonyOS SDK（DEVECO_SDK_HOME / HOS_SDK_HOME 均未设置）。\n' +
        '  HarmonyOS 出包需要安装 DevEco Studio 5.0+ 并配置 HarmonyOS SDK（API 12），\n' +
        '  且需在 AppScope/build-profile.json5 的 signingConfigs 中配置签名后才能安装到真机。',
    )
  }

  const hvigor = findHvigor()
  const task = mode === 'debug' ? 'assembleHap' : 'assembleHap'
  const args = [
    task,
    '--mode',
    'module',
    '-p',
    'product=default',
    '-p',
    `buildMode=${mode}`,
  ]

  console.log(`[harmony] ${hvigor} ${args.join(' ')}`)
  const r = spawnSync(hvigor, args, { cwd: PKG_DIR, stdio: 'inherit', shell: process.platform === 'win32' })
  if (r.error) throw new Error(`无法执行 hvigorw：${r.error.message}`)
  if (r.status !== 0) throw new Error(`hvigorw 退出码 ${r.status}`)

  // 收集 HAP 产物并核验版本号
  const haps = []
  const walk = (d, depth = 0) => {
    if (!fs.existsSync(d) || depth > 8) return
    for (const n of fs.readdirSync(d)) {
      const p = path.join(d, n)
      const st = fs.statSync(p)
      if (st.isDirectory()) walk(p, depth + 1)
      else if (/\.(hap|hsp)$/i.test(n)) haps.push(p)
    }
  }
  walk(path.join(PKG_DIR, 'entry', 'build'))

  console.log(`\n[harmony] 产物：`)
  if (haps.length === 0) {
    console.log('  未发现 .hap —— hvigor 可能把产物放在了别的路径，请查看上方输出')
  }
  const needle = Buffer.from(version.canonical, 'utf8')
  for (const h of haps) {
    const f = fs.statSync(h)
    const hit = fs.readFileSync(h).includes(needle)
    console.log(
      `  ${path.relative(REPO, h)}  ${(f.size / 1024 / 1024).toFixed(1)} MB  版本号${hit ? '已检出' : '未检出'}`,
    )
  }
}

try {
  main()
} catch (e) {
  console.error(`\n[harmony] 失败：${e.message}`)
  process.exit(1)
}
