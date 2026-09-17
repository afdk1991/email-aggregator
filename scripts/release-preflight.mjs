#!/usr/bin/env node
/**
 * ============================================================================
 *  Release 附件预检（发布前门禁）—— 项目003「邮箱聚合平台」
 * ============================================================================
 *
 *  为什么需要它：GitHub Release 一创建就对全网可见（本仓库是 PUBLIC），
 *  附件传错、传烂、传重都**来不及撤回**。所以在 `softprops/action-gh-release`
 *  之前，把下面这些事全部钉死：
 *
 *   ① 目录束必须先打包
 *      iOS 产物是 `App.xcarchive` —— 一个**目录束**。actions/upload-artifact
 *      会保留整棵树，于是 `files: dist/**` 会把归档内部的**每一个**文件
 *      （dSYMs、.swiftmodule、framework 分片…）都当成独立附件上传：
 *      结果是几百个垃圾附件 + 一个结构被拆散、再也装不回 Xcode 的归档。
 *      本脚本一旦发现残留目录束就**直接失败**，逼着流水线在各自的宿主平台上
 *      先压成单文件（iOS 用 macOS 原生 `ditto -c -k --keepParent`）。
 *
 *   ② 附件重名必须提前暴露
 *      action-gh-release 遇到同名附件是**静默覆盖**：不同 job 产出的同名文件
 *      最终只有一个能进 Release，而且不报错、不警告。本脚本扁平化时对
 *      basename 做全局去重，冲突则列出全部来源路径并失败。
 *
 *   ③ `.zip` 必须真的是 ZIP
 *      Release 附件里既有 macOS 的 electron zip，也有 iOS 的 xcarchive zip。
 *      若打包环节悄悄只写出半个文件，发出来才发现就晚了。这里复用
 *      platforms/mobile/scripts/zip-search.mjs 的读取器做结构级校验
 *      （EOCD → 中央目录 → 每个条目的本地文件头），不引入第三方依赖。
 *
 *   ④ 数量与体积上限
 *      GitHub Release 单次最多 1000 个附件、单个附件最大 2 GiB。超限是硬失败，
 *      且不存在"部分成功"的补救空间。
 *
 *  用法：
 *    node scripts/release-preflight.mjs [--dist dist] [--flat dist/_assets]
 *                                       [--manifest dist/ASSETS.txt]
 *    node scripts/release-preflight.mjs --selftest
 *
 *  退出码：0 = 可以发布；1 = 预检失败（禁止创建 Release）；2 = 参数/环境错误。
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { readZipEntries, readZipEntry } from '../platforms/mobile/scripts/zip-search.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const ROOT = path.resolve(HERE, '..')

/** GitHub Release 的硬上限，超了没有补救空间。 */
const MAX_ASSETS = 1000
const MAX_ASSET_BYTES = 2 * 1024 ** 3

/**
 * 目录束扩展名。这些都是"一个目录、但语义上是一个文件"的 macOS/Xcode 包，
 * 必须先在宿主平台上压成单文件才能当 Release 附件。
 */
const BUNDLE_EXTS = ['.xcarchive', '.app', '.dSYMs', '.framework', '.bundle', '.xcodeproj', '.playground']

/** 覆盖面自检：六族产物缺哪族就提醒（不算失败，只有"一个附件都没有"才算失败）。 */
const FAMILIES = [
  { label: 'Windows 安装包', test: (n) => /\.exe$/i.test(n) },
  { label: 'macOS 磁盘映像', test: (n) => /\.dmg$/i.test(n) },
  { label: 'Linux 便携包', test: (n) => /\.AppImage$/i.test(n) },
  { label: 'Android 安装包', test: (n) => /\.apk$/i.test(n) },
  { label: 'iOS 归档', test: (n) => /\.xcarchive\.zip$/i.test(n) },
  { label: 'Go 单二进制', test: (n) => /^email-aggregator-(linux|darwin|windows)-/i.test(n) },
]

const fail = (msg) => {
  process.stderr.write(`[release] ${msg}\n`)
  process.exit(2)
}
const out = (s = '') => process.stdout.write(s + '\n')
const warn = (s) => process.stdout.write(`  WARN   ${s}\n`)
const bad = (s) => process.stdout.write(`  FAIL   ${s}\n`)
const good = (s) => process.stdout.write(`  OK     ${s}\n`)
const mib = (n) => `${(n / 1024 / 1024).toFixed(1)} MB`

// ============================================================================
//  参数
// ============================================================================

function parseArgs(argv) {
  const args = {}
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a === '--selftest') {
      args.selftest = true
      continue
    }
    if (a.startsWith('--')) {
      const key = a.slice(2)
      const next = argv[i + 1]
      args[key] = next && !next.startsWith('--') ? argv[++i] : 'true'
      continue
    }
    fail(`未知参数 "${a}"`)
  }
  return args
}

// ============================================================================
//  扫描
// ============================================================================

/**
 * 递归列目录内容。skipRoots 命中的子树整体跳过（用于排除本脚本自己的输出目录，
 * 否则第二次运行会把上一次的扁平化结果也当成产物）。
 */
function walk(root, skipRoots, skipFiles) {
  const files = []
  const dirs = []
  const others = []
  const stack = [root]
  while (stack.length) {
    const d = stack.pop()
    let items
    try {
      items = fs.readdirSync(d, { withFileTypes: true })
    } catch (e) {
      others.push(`${d}（无法读取：${e.message}）`)
      continue
    }
    for (const it of items) {
      const p = path.join(d, it.name)
      const abs = path.resolve(p)
      if (it.isDirectory()) {
        if (skipRoots.has(abs)) continue
        dirs.push(abs)
        stack.push(p)
      } else if (it.isFile()) {
        if (skipFiles.has(abs)) continue
        files.push({ p: abs, size: fs.statSync(p).size })
      } else {
        // 符号链接/设备文件等：Release 附件不该出现，报出来让人确认。
        others.push(abs)
      }
    }
  }
  return { files, dirs, others }
}

/**
 * ZIP 结构级校验：EOCD 必须能找到、中央目录必须非空、每个条目的本地文件头
 * 必须落在文件内且签名正确。只做结构校验，不校验 CRC（读取器的定位如此）。
 */
function validateZip(file, buf) {
  const entries = readZipEntries(buf)
  if (!entries || entries.length === 0) return { ok: false, reason: 'EOCD 或中央目录不可解析（不是 ZIP？）' }
  for (const e of entries) {
    if (e.localOff + 30 > buf.length) return { ok: false, reason: `条目 "${e.name}" 的本地头越界` }
    if (buf.readUInt32LE(e.localOff) !== 0x04034b50) {
      return { ok: false, reason: `条目 "${e.name}" 的本地头签名错误` }
    }
  }
  return { ok: true, entries: entries.length }
}

// ============================================================================
//  预检主体
// ============================================================================

export function preflight({ dist, flat, manifest, quiet = false }) {
  const problems = []
  const warnings = []
  const log = quiet ? () => {} : out

  if (!fs.existsSync(dist)) fail(`--dist 目录不存在：${dist}`)
  if (!fs.statSync(dist).isDirectory()) fail(`--dist 不是目录：${dist}`)

  const flatAbs = path.resolve(flat)
  // 排除本脚本自己的两处输出（扁平目录、清单文件）：否则二次运行会把上一次的
  // 结果当成新产物，重名检测会莫名其妙地报警。
  const skipFiles = new Set(manifest ? [path.resolve(manifest)] : [])
  const { files, dirs, others } = walk(dist, new Set([flatAbs]), skipFiles)

  log(`[release] Release 附件预检`)
  log(`  dist      ${dist}`)
  log(`  flat      ${flatAbs}`)
  log(`  扫描到    ${files.length} 个文件 / ${dirs.length} 个目录`)
  log('')

  // ── ① 目录束必须已经打包 ──────────────────────────────────────────────
  const bundleDirs = dirs.filter((d) => {
    const lower = d.toLowerCase()
    return BUNDLE_EXTS.some((ext) => lower.endsWith(ext.toLowerCase()))
  })
  if (bundleDirs.length > 0) {
    for (const d of bundleDirs) bad(`残留目录束，必须先压成单文件：${path.relative(dist, d)}`)
    problems.push(
      `发现 ${bundleDirs.length} 个未打包的目录束。它们在 Release 里会被拆成成百上千个附件，` +
        `归档本身反而不可用。请在上游 job 里先打包（iOS 用 ditto -c -k --keepParent）。`,
    )
  } else {
    good('无残留目录束')
  }

  // ── ② ZIP 必须结构完整 ────────────────────────────────────────────────
  const zips = files.filter((f) => /\.zip$/i.test(f.p))
  let zipChecked = 0
  for (const f of zips) {
    let buf
    try {
      buf = fs.readFileSync(f.p)
    } catch (e) {
      bad(`读取失败：${path.relative(dist, f.p)} —— ${e.message}`)
      problems.push(`无法读取 ${path.relative(dist, f.p)}`)
      continue
    }
    const v = validateZip(f.p, buf)
    const rel = path.relative(dist, f.p)
    if (!v.ok) {
      bad(`不是结构完整的 ZIP：${rel} —— ${v.reason}`)
      problems.push(`${rel} 不是结构完整的 ZIP（${v.reason}）`)
      // 继续检查别的 zip，一次把问题报全
      continue
    }
    // xcarchive 归档额外要求：必须还能找得到 Info.plist，证明压的是真归档而
    // 不是一个被截断的空壳。
    if (/\.xcarchive\.zip$/i.test(f.p)) {
      const hasPlist = readZipEntries(buf).some((e) => /Info\.plist$/i.test(e.name))
      if (!hasPlist) {
        bad(`归档内缺 Info.plist：${rel}`)
        problems.push(`${rel} 内没有 Info.plist —— 压出来的不是有效 xcarchive`)
        continue
      }
      good(`${rel}  ${mib(f.size)}  ZIP 结构完整（${v.entries} 条目，含 Info.plist）`)
    } else {
      good(`${rel}  ${mib(f.size)}  ZIP 结构完整（${v.entries} 条目）`)
    }
    zipChecked++
  }
  if (zips.length === 0) warnings.push('dist 内没有任何 .zip 附件（macOS zip / iOS 归档是否漏传？）')
  else if (zipChecked === zips.length) log('')

  // ── ③ 扁平化 + 重名检测 ──────────────────────────────────────────────
  if (others.length > 0) {
    for (const o of others) warn(`非常规文件类型（符号链接/设备等）：${path.relative(dist, o)}`)
    warnings.push(`dist 内有 ${others.length} 个非常规文件，请确认不是遗漏的产物`)
  }

  const byName = new Map()
  for (const f of files) {
    const base = path.basename(f.p)
    if (!byName.has(base)) byName.set(base, [])
    byName.get(base).push(f)
  }
  const dupes = [...byName.entries()].filter(([, list]) => list.length > 1)
  for (const [name, list] of dupes) {
    bad(`附件重名（action-gh-release 会静默覆盖，只留一个）：${name}`)
    for (const f of list) log(`           ← ${path.relative(dist, f.p)}`)
  }
  if (dupes.length > 0) {
    problems.push(
      `${dupes.length} 个附件 basename 冲突。请在各 job 的 artifactName/产物命名里加上平台或架构区分。`,
    )
  } else if (files.length > 0) {
    good(`${files.length} 个附件命名互不冲突`)
  }

  // ── ④ 体积与数量 ────────────────────────────────────────────────────
  let total = 0
  for (const f of files) {
    const rel = path.relative(dist, f.p)
    if (f.size === 0) {
      bad(`0 字节附件：${rel}`)
      problems.push(`${rel} 是 0 字节`)
    } else if (f.size > MAX_ASSET_BYTES) {
      bad(`超过单附件 2 GiB 上限：${rel}  ${mib(f.size)}`)
      problems.push(`${rel} 超过 2 GiB`)
    }
    total += f.size
  }
  if (files.length > MAX_ASSETS) {
    bad(`附件数 ${files.length} 超过 GitHub Release 上限 ${MAX_ASSETS}`)
    problems.push(`附件数 ${files.length} > ${MAX_ASSETS}`)
  } else if (files.length > 0 && problems.length === 0) {
    good(`${files.length} 个附件，合计 ${mib(total)}，均在上限内`)
  }

  // ── ⑤ 覆盖面 ────────────────────────────────────────────────────────
  if (files.length === 0) {
    problems.push('dist 内一个附件都没有 —— download-artifact 是否拿到了任何产物？')
    bad('附件总数为 0')
  } else {
    const names = [...byName.keys()]
    const missing = FAMILIES.filter((fam) => !names.some((n) => fam.test(n)))
    for (const fam of missing) warn(`未发现${fam.label}产物（该 job 可能被跳过或上传路径变更）`)
    if (missing.length === 0) good(`六族产物齐全（Windows/macOS/Linux/Android/iOS/Go）`)
  }

  // ── 输出清单 ────────────────────────────────────────────────────────
  const assets = files.map((f) => f.p).sort()
  return { problems, warnings, assets, totalBytes: total }
}

// ============================================================================
//  入口
// ============================================================================

function main() {
  const args = parseArgs(process.argv.slice(2))

  if (args.selftest) return selftest()

  const dist = path.resolve(ROOT, args.dist ?? 'dist')
  const flat = path.resolve(ROOT, args.flat ?? path.join(args.dist ?? 'dist', '_assets'))
  // 清单默认落在 dist 内部 —— .gitignore 已忽略 `dist/`，这样本地跑完不会弄脏工作树。
  const manifest = path.resolve(ROOT, args.manifest ?? path.join(args.dist ?? 'dist', 'ASSETS.txt'))

  const { problems, warnings, assets, totalBytes } = preflight({ dist, flat, manifest })

  if (problems.length === 0) {
    // 只有确定可以发布时才准备扁平目录 —— 避免"预检失败但目录已被清空"的误导。
    fs.rmSync(flat, { recursive: true, force: true })
    fs.mkdirSync(flat, { recursive: true })
    for (const a of assets) fs.copyFileSync(a, path.join(flat, path.basename(a)))

    fs.writeFileSync(manifest, assets.join('\n') + '\n', 'utf8')
    out('')
    out(`[release] 附件清单已写入 ${path.relative(ROOT, manifest)}，扁平目录 ${path.relative(ROOT, flat)}`)
    out(`  ${assets.length} 个附件，合计 ${mib(totalBytes)}`)
    for (const a of assets) out(`    ${path.basename(a)}`)
    if (warnings.length > 0) {
      out('')
      out(`[release] ${warnings.length} 条提醒（不阻断发布）`)
    }
    out('')
    out('[release] 预检通过 —— 可以创建 Release')

    writeGithubOutput({ count: String(assets.length), flat })
    return
  }

  out('')
  out(`[release] 预检失败，共 ${problems.length} 项：`)
  for (const p of problems) out(`  · ${p}`)
  out('')
  out('[release] 禁止创建 Release。修好上游产物再重跑。')
  process.exitCode = 1
}

/** 有 GITHUB_OUTPUT 时把计数下发，便于工作流回显（空串则跳过，自测时会用到）。 */
function writeGithubOutput(values) {
  const file = process.env.GITHUB_OUTPUT
  if (!file) return
  const lines = Object.entries(values).map(([k, v]) => `${k}=${v}`)
  fs.appendFileSync(file, lines.join('\n') + '\n', 'utf8')
}

// ============================================================================
//  自测（--selftest）
//
//  夹具在临时目录里现造，不依赖任何真实构建产物。六个用例全部断言**退出码**与
//  关键报错片段 —— 尤其是四个"必须失败"的用例，它们对应的正是会让公开 Release
//  半残却又不报错的四种情形。
// ============================================================================

/** 只需要 stored 条目，所以 CRC32 表就地实现，不引入依赖。 */
const CRC_TABLE = (() => {
  const t = new Int32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    t[n] = c
  }
  return t
})()

function crc32(buf) {
  let c = -1
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8)
  return (c ^ -1) >>> 0
}

/** 造一个最小但结构合法的 ZIP（全部 stored，无压缩）。 */
function buildStoredZip(entries) {
  const chunks = []
  const central = []
  let offset = 0
  for (const e of entries) {
    const name = Buffer.from(e.name, 'utf8')
    const data = Buffer.isBuffer(e.data) ? e.data : Buffer.from(e.data, 'utf8')
    const crc = crc32(data)
    const lh = Buffer.alloc(30)
    lh.writeUInt32LE(0x04034b50, 0)
    lh.writeUInt16LE(20, 4)
    lh.writeUInt16LE(0x0800, 6) // UTF-8 文件名
    lh.writeUInt16LE(0, 8) // stored
    lh.writeUInt16LE(0, 10) // mod time
    lh.writeUInt16LE(0x21, 12) // mod date = 1980-01-01
    lh.writeUInt32LE(crc, 14)
    lh.writeUInt32LE(data.length, 18)
    lh.writeUInt32LE(data.length, 22)
    lh.writeUInt16LE(name.length, 26)
    lh.writeUInt16LE(0, 28)
    chunks.push(lh, name, data)
    central.push({ name, crc, size: data.length, offset })
    offset += 30 + name.length + data.length
  }

  const cdChunks = []
  let cdSize = 0
  for (const c of central) {
    const ch = Buffer.alloc(46)
    ch.writeUInt32LE(0x02014b50, 0)
    ch.writeUInt16LE(20, 4) // version made by
    ch.writeUInt16LE(20, 6) // version needed
    ch.writeUInt16LE(0x0800, 8)
    ch.writeUInt16LE(0, 10)
    ch.writeUInt16LE(0, 12)
    ch.writeUInt16LE(0x21, 14)
    ch.writeUInt32LE(c.crc, 16)
    ch.writeUInt32LE(c.size, 20)
    ch.writeUInt32LE(c.size, 24)
    ch.writeUInt16LE(c.name.length, 28)
    ch.writeUInt16LE(0, 30)
    ch.writeUInt16LE(0, 32)
    ch.writeUInt16LE(0, 34)
    ch.writeUInt16LE(0, 36)
    ch.writeUInt32LE(0, 38)
    ch.writeUInt32LE(c.offset, 42)
    cdChunks.push(ch, c.name)
    cdSize += 46 + c.name.length
  }

  const eocd = Buffer.alloc(22)
  eocd.writeUInt32LE(0x06054b50, 0)
  eocd.writeUInt16LE(central.length, 8)
  eocd.writeUInt16LE(central.length, 10)
  eocd.writeUInt32LE(cdSize, 12)
  eocd.writeUInt32LE(offset, 16)

  return Buffer.concat([...chunks, ...cdChunks, eocd])
}

function selftest() {
  const tmp = fs.mkdtempSync(path.join(process.env.TEMP ?? '/tmp', 'p003-relpre-'))
  const script = fileURLToPath(import.meta.url)
  let pass = 0
  let failed = 0

  const check = (label, cond, detail = '') => {
    if (cond) {
      pass++
      out(`  PASS  ${label}`)
    } else {
      failed++
      out(`  FAIL  ${label}${detail ? `  —— ${detail}` : ''}`)
    }
  }

  /** 造一个 dist 夹具并跑一遍预检，返回 { code, stdout }。 */
  const run = (name, build) => {
    const dist = path.join(tmp, name)
    fs.mkdirSync(dist, { recursive: true })
    build(dist)
    const r = spawnSync(
      process.execPath,
      [script, '--dist', dist, '--flat', path.join(dist, '_assets'), '--manifest', path.join(dist, 'ASSETS.txt')],
      { encoding: 'utf8', env: { ...process.env, GITHUB_OUTPUT: '' } },
    )
    return { code: r.status, stdout: `${r.stdout ?? ''}${r.stderr ?? ''}`, dist }
  }

  const xcarchiveZip = () =>
    buildStoredZip([
      { name: 'App.xcarchive/Info.plist', data: '<plist><dict/></plist>' },
      { name: 'App.xcarchive/Products/Applications/App.app/App', data: 'binary-bytes' },
    ])

  out('[release] 附件预检自测')
  out('')

  // ① 正常：六族齐全，xcarchive 已打成 zip
  {
    const { code, stdout, dist } = run('ok', (d) => {
      const put = (dir, file, data) => {
        fs.mkdirSync(path.join(d, dir), { recursive: true })
        fs.writeFileSync(path.join(d, dir, file), data)
      }
      put('desktop-windows-MALLV0.0.0', 'EmailAggregator-MALLV0.0.0-win-x64.exe', Buffer.alloc(2048, 1))
      put('desktop-macos-MALLV0.0.0', 'EmailAggregator-MALLV0.0.0-mac-arm64.dmg', Buffer.alloc(2048, 2))
      put('desktop-linux-MALLV0.0.0', 'EmailAggregator-MALLV0.0.0-linux-x86_64.AppImage', Buffer.alloc(2048, 3))
      put('android-MALLV0.0.0', 'app-debug.apk', Buffer.alloc(2048, 4))
      put('ios-MALLV0.0.0', 'App.xcarchive.zip', xcarchiveZip())
      put('go-server-MALLV0.0.0', 'email-aggregator-linux-amd64', Buffer.alloc(2048, 5))
    })
    check('正常产物：预检通过（退出码 0）', code === 0, `实际退出码 ${code}`)
    check('正常产物：六族齐全被识别', /六族产物齐全/.test(stdout))
    check('正常产物：无残留目录束', /无残留目录束/.test(stdout))
    const flat = fs.readdirSync(path.join(dist, '_assets'))
    check('正常产物：扁平目录落 6 个附件', flat.length === 6, `实际 ${flat.length}：${flat.join(', ')}`)
    check('正常产物：清单写入 ASSETS.txt', fs.readFileSync(path.join(dist, 'ASSETS.txt'), 'utf8').trim().split('\n').length === 6)
  }

  // ② 残留目录束（本条对应的正是 release job 原先的必挂缺陷）
  {
    const { code, stdout } = run('bundle', (d) => {
      const dir = path.join(d, 'ios-MALLV0.0.0', 'App.xcarchive', 'Products')
      fs.mkdirSync(dir, { recursive: true })
      fs.writeFileSync(path.join(dir, 'Info.plist'), '<plist/>')
      fs.mkdirSync(path.join(d, 'android-MALLV0.0.0'), { recursive: true })
      fs.writeFileSync(path.join(d, 'android-MALLV0.0.0', 'app-debug.apk'), Buffer.alloc(64, 1))
    })
    check('残留目录束：必须失败', code === 1, `实际退出码 ${code}`)
    check('残留目录束：报出 xcarchive 路径', /残留目录束/.test(stdout) && /App\.xcarchive/.test(stdout))
    check('残留目录束：不再生成扁平目录', !fs.existsSync(path.join(tmp, 'bundle', '_assets')))
  }

  // ③ 附件重名（action-gh-release 会静默覆盖）
  {
    const { code, stdout } = run('dupe', (d) => {
      for (const dir of ['desktop-macos-MALLV0.0.0', 'desktop-linux-MALLV0.0.0']) {
        fs.mkdirSync(path.join(d, dir), { recursive: true })
        fs.writeFileSync(path.join(d, dir, 'latest.zip'), Buffer.alloc(64, 1))
      }
    })
    check('附件重名：必须失败', code === 1, `实际退出码 ${code}`)
    check('附件重名：列出两个来源', (stdout.match(/←/g) ?? []).length === 2)
  }

  // ④ 伪 ZIP（扩展名对、内容不是 ZIP）
  {
    const { code, stdout } = run('fakezip', (d) => {
      fs.mkdirSync(path.join(d, 'ios-MALLV0.0.0'), { recursive: true })
      fs.writeFileSync(path.join(d, 'ios-MALLV0.0.0', 'App.xcarchive.zip'), Buffer.from('这不是 ZIP，只是恰好叫 .zip'))
    })
    check('伪 ZIP：必须失败', code === 1, `实际退出码 ${code}`)
    check('伪 ZIP：给出结构原因', /不是结构完整的 ZIP/.test(stdout))
  }

  // ⑤ xcarchive.zip 里没有 Info.plist
  {
    const { code, stdout } = run('noplist', (d) => {
      fs.mkdirSync(path.join(d, 'ios-MALLV0.0.0'), { recursive: true })
      fs.writeFileSync(
        path.join(d, 'ios-MALLV0.0.0', 'App.xcarchive.zip'),
        buildStoredZip([{ name: 'App.xcarchive/Products/x.bin', data: 'x' }]),
      )
    })
    check('归档缺 Info.plist：必须失败', code === 1, `实际退出码 ${code}`)
    check('归档缺 Info.plist：给出原因', /缺 Info\.plist/.test(stdout))
  }

  // ⑥ 0 字节附件
  {
    const { code, stdout } = run('zero', (d) => {
      fs.mkdirSync(path.join(d, 'desktop-windows-MALLV0.0.0'), { recursive: true })
      fs.writeFileSync(path.join(d, 'desktop-windows-MALLV0.0.0', 'EmailAggregator-MALLV0.0.0-win-x64.exe'), '')
      fs.writeFileSync(path.join(d, 'desktop-windows-MALLV0.0.0', 'ok.dmg'), Buffer.alloc(8, 1))
    })
    check('0 字节附件：必须失败', code === 1, `实际退出码 ${code}`)
    check('0 字节附件：点名该文件', /0 字节附件.*\.exe/.test(stdout))
  }

  // ⑦ 空 dist
  {
    const { code, stdout } = run('empty', () => {})
    check('空 dist：必须失败', code === 1, `实际退出码 ${code}`)
    check('空 dist：提示附件总数为 0', /附件总数为 0/.test(stdout))
  }

  fs.rmSync(tmp, { recursive: true, force: true })

  out('')
  out(`[release] 自测 ${pass}/${pass + failed} 通过`)
  if (failed > 0) process.exitCode = 1
}

main()
