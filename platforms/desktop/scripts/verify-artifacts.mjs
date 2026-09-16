#!/usr/bin/env node
/**
 * ============================================================================
 *  桌面端产物校验（verify-artifacts）
 * ============================================================================
 *
 *  回答一个具体问题：**打出来的安装包，是不是真的带上了 version.json 里的版本号？**
 *
 *  光看文件名不够 —— 文件名由 ${env.MALLV_VERSION} 宏拼出，只能证明「字符串传进去了」，
 *  不能证明「运行时读到的版本也是它」。所以这里做三层证据链：
 *
 *    ① 文件名层：release/ 下每个安装包名都含 MALLVx.y.z
 *    ② 目录层：  未打包目录的资源目录里存在 sidecar 与 app-dist
 *                · Windows/Linux：`<outDir>/resources/`
 *                · macOS：        `<outDir>/<ProductName>.app/Contents/Resources/`
 *    ③ 内容层：  · sidecar 二进制内含 MALLVx.y.z（Go 的 src/version 编译进去了）
 *                · app-dist/index.html 含 <meta name="app-version" content="MALLVx.y.z">
 *                · asar 内的 version.generated.cjs 与 version.json 一致
 *
 *  用法：node scripts/verify-artifacts.mjs
 *        node scripts/verify-artifacts.mjs --dir release/win-unpacked
 *  退出码：0 全通过；1 有任一失败（可直接作为 CI 门禁）
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const PKG_DIR = path.resolve(HERE, '..')
const REPO = path.resolve(PKG_DIR, '..', '..')
const RELEASE = path.join(PKG_DIR, 'release')
const VERSION_TOOL = path.join(REPO, 'scripts', 'version.mjs')

const argv = process.argv.slice(2)
const dirArg = argv.includes('--dir') ? argv[argv.indexOf('--dir') + 1] : null

const checks = []
const pass = (name, detail) => checks.push({ ok: true, name, detail })
const failCheck = (name, detail) => checks.push({ ok: false, name, detail })

function readVersion() {
  const r = spawnSync(process.execPath, [VERSION_TOOL, 'show', '--json'], { encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`无法读取版本：${r.stderr || r.stdout}`)
  return JSON.parse(r.stdout)
}

function walk(dir, base = dir) {
  const out = []
  if (!fs.existsSync(dir)) return out
  for (const name of fs.readdirSync(dir)) {
    const abs = path.join(dir, name)
    const st = fs.statSync(abs)
    if (st.isDirectory()) out.push(...walk(abs, base))
    else out.push({ rel: path.relative(base, abs), abs, size: st.size })
  }
  return out
}

function main() {
  const v = readVersion()
  const needle = v.canonical
  console.log(`[verify] 期望版本 ${needle}（semver=${v.semver}, code=${v.code}）`)
  console.log(`[verify] 产物目录 ${dirArg ?? path.relative(REPO, RELEASE)}\n`)

  // ── ① 文件名层 ─────────────────────────────────────────────────────────
  const root = dirArg ? path.resolve(PKG_DIR, dirArg) : RELEASE
  if (!fs.existsSync(root)) {
    console.log(`[verify] 产物目录不存在：${root}`)
    console.log('[verify] 请先构建：node scripts/build.mjs --win （或 --mac / --linux）')
    process.exit(1)
  }

  // 安装包 = 根目录下带扩展名的文件（不含 unpacked 目录）
  const installers = fs
    .readdirSync(root)
    .map((n) => path.join(root, n))
    .filter((p) => fs.statSync(p).isFile())
    .filter((p) => /\.(exe|dmg|zip|AppImage|deb|rpm|snap|pkg|msi|blockmap|yml|yaml|json|7z|tar\.gz)$/i.test(p))

  const pkgs = installers.filter((p) => /\.(exe|dmg|AppImage|deb|rpm|snap|pkg|msi|zip)$/i.test(p))
  if (pkgs.length === 0) {
    failCheck('安装包存在', `${path.relative(REPO, root)} 下未发现安装包文件`)
  } else {
    const bad = pkgs.filter((p) => !path.basename(p).includes(needle))
    if (bad.length === 0) {
      pass('安装包文件名含规范版本号', `${pkgs.length} 个产物全部含 ${needle}`)
      for (const p of pkgs) {
        console.log(`        ${path.basename(p)}  ${(fs.statSync(p).size / 1024 / 1024).toFixed(1)} MB`)
      }
    } else {
      failCheck(
        '安装包文件名含规范版本号',
        `以下产物名不含 ${needle}：\n        ${bad.map((p) => path.basename(p)).join('\n        ')}`,
      )
    }
  }

  // ── ② + ③ 目录层与内容层（需要 unpacked 目录）───────────────────────────
  const unpacked =
    fs.existsSync(path.join(root, 'win-unpacked'))
      ? path.join(root, 'win-unpacked')
      : fs.existsSync(path.join(root, 'mac'))
        ? path.join(root, 'mac')
        : fs.existsSync(path.join(root, 'mac-arm64'))
          ? path.join(root, 'mac-arm64')
          : fs.existsSync(path.join(root, 'linux-unpacked'))
            ? path.join(root, 'linux-unpacked')
            : null

  if (dirArg) {
    // 显式指定了 --dir，则把它本身当作 unpacked 目录
    verifyUnpacked(path.resolve(PKG_DIR, dirArg), v)
  } else if (!unpacked) {
    console.log('[verify] 未发现 *-unpacked 目录（安装包可能已被移动到别处），跳过内容层校验')
  } else {
    verifyUnpacked(unpacked, v)
  }

  // ── 汇总 ───────────────────────────────────────────────────────────────
  console.log('')
  const failed = checks.filter((c) => !c.ok)
  for (const c of checks) console.log(`  ${c.ok ? 'OK  ' : 'FAIL'}  ${c.name}\n        ${c.detail}`)
  console.log('')
  if (failed.length) {
    console.log(`[verify] ${failed.length} 项失败`)
    process.exit(1)
  }
  console.log(`[verify] 全部通过 —— 安装包确实携带 ${needle}`)
}

/**
 * 定位「未打包目录」里的资源目录。
 *
 * Windows/Linux 是 `<outDir>/resources`；**macOS 不是** —— 它是
 * `<outDir>/<ProductName>.app/Contents/Resources`。此前本函数无条件拼 `resources`，
 * 导致 macOS 在产物全部正确产出的情况下仍报 `resources 目录存在` FAIL
 * （实测 run 35118604475：4 个 dmg/zip 均正常，只有这项检查失败）。
 * 按两种形态依次探测，而不是按 platform 硬编码 —— 后者会在
 * `--dir` 手动指定目录时失配。
 */
function resolveResources(unpackedDir) {
  const generic = path.join(unpackedDir, 'resources')
  if (fs.existsSync(generic)) return generic
  if (fs.existsSync(unpackedDir)) {
    for (const name of fs.readdirSync(unpackedDir)) {
      if (!name.endsWith('.app')) continue
      const macRes = path.join(unpackedDir, name, 'Contents', 'Resources')
      if (fs.existsSync(macRes)) return macRes
    }
  }
  return generic
}

function verifyUnpacked(unpackedDir, v) {
  const res = resolveResources(unpackedDir)
  if (!fs.existsSync(res)) {
    failCheck(
      'resources 目录存在',
      `${path.relative(REPO, res)} 不存在（期望 Windows/Linux 的 resources/，或 macOS 的 <ProductName>.app/Contents/Resources）`,
    )
    return
  }
  console.log(`[verify] 检查未打包目录 ${path.relative(REPO, unpackedDir)}`)

  // sidecar
  const binDir = path.join(res, 'bin')
  const sidecars = fs.existsSync(binDir) ? fs.readdirSync(binDir) : []
  if (sidecars.length === 0) {
    failCheck('随包分发 Go sidecar', `${path.relative(REPO, binDir)} 为空 —— 应用将只能降级为只读模式`)
  } else {
    const bad = []
    for (const name of sidecars) {
      const abs = path.join(binDir, name)
      const buf = fs.readFileSync(abs)
      if (!buf.includes(Buffer.from(v.canonical, 'utf8'))) bad.push(name)
    }
    if (bad.length) failCheck('sidecar 内嵌版本号', `以下二进制未检出 ${v.canonical}：${bad.join(', ')}`)
    else pass('sidecar 内嵌版本号', `${sidecars.join(', ')} 均含 ${v.canonical}`)
  }

  // 静态兜底产物
  const indexHtml = path.join(res, 'app-dist', 'index.html')
  if (!fs.existsSync(indexHtml)) {
    failCheck('静态兜底页面', `${path.relative(REPO, indexHtml)} 不存在`)
  } else {
    const html = fs.readFileSync(indexHtml, 'utf8')
    if (html.includes(`content="${v.canonical}"`)) pass('静态页面版本元信息', `index.html 含 ${v.canonical}`)
    else failCheck('静态页面版本元信息', `index.html 未含 content="${v.canonical}"`)
  }

  // asar 内的版本模块（asar 是简单归档，版本字符串以明文存储，可直接检索）
  const asar = path.join(res, 'app.asar')
  if (fs.existsSync(asar)) {
    const buf = fs.readFileSync(asar)
    if (buf.includes(Buffer.from(v.canonical, 'utf8'))) pass('app.asar 内版本常量', `检出 ${v.canonical}`)
    else failCheck('app.asar 内版本常量', `未检出 ${v.canonical}（version.generated.cjs 可能未打包）`)
  } else {
    console.log('[verify] 未发现 app.asar（asar 已被禁用？），跳过该项')
  }
}

main()
