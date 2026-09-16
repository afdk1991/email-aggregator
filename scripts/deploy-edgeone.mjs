#!/usr/bin/env node
/**
 * ============================================================================
 *  项目003「邮箱聚合平台」· EdgeOne Makers 官网发布器（单一实现）
 * ============================================================================
 *
 *  为什么必须有这个文件：
 *    官网发布原本有三份并行实现 —— deploy/edgeone-app/deploy.ps1（Windows）、
 *    deploy.sh（macOS/Linux）、.github/workflows/deploy-edgeone.yml（CI）。
 *    三者步骤顺序与同步语义各不相同：两个 shell 脚本手工 cp 产物，CI 也手工 cp，
 *    且都不清理陈旧资源、都不做版本门禁。本项目已两次因「同一件事多处实现」而
 *    漏同步（漏 CSS、漏 webroot/dist，线上跑的是旧包）。
 *
 *    故收敛为一份 Node 实现：本地与 CI 走**完全相同的代码路径**，
 *    只有"谁来调用它"不同。选 Node 而非 shell 的理由与 version.mjs 一致：
 *    macOS/Linux CI runner 无 pwsh，而本项目的 .ps1 曾因编码问题长期不可运行。
 *
 *  六个环节（顺序即依赖，任一失败即中止，不产出「部分成功」）：
 *    ① 版本门禁    node scripts/version.mjs sync && verify —— 21 个落点漂移即中止
 *    ② 前端构建    email-aggregator-web 的 npm run build（产出 dist/）
 *    ③ 产物同步    scripts/sync-web-assets.mjs 的五处目标 + 复验
 *                  （逐文件 SHA-256，并清除目标侧陈旧残留）
 *    ④ 项目绑定    deploy/edgeone-app/edgeone-project.json → .edgeone/project.json
 *                  CLI 会按**项目名**远程解析（-n 才是权威），绑定文件与 -n 不同名
 *                  时 CLI 会顺手另建一个新项目，这是最贵的坑
 *    ⑤ 冒烟 + 部署 node smoke.mjs 演示态 / --prod 生产态各一套 → edgeone makers deploy
 *    ⑥ 线上验证    --verify-live：curl 驱动跑 scripts/live-verify.mjs 全接口断言
 *
 *  用法：
 *    node scripts/deploy-edgeone.mjs                  # 全流程（含前端构建）
 *    node scripts/deploy-edgeone.mjs --skip-build     # 复用已有 dist（CI 已构建）
 *    node scripts/deploy-edgeone.mjs --dry-run        # 做到冒烟为止，不真正部署
 *    node scripts/deploy-edgeone.mjs --verify-live    # 部署后跑线上全接口验证
 *    node scripts/deploy-edgeone.mjs --json           # 机器可读输出（供 CI 消费）
 *    node scripts/deploy-edgeone.mjs --project other  # 指定项目名（默认读绑定文件）
 *
 *  环境变量：
 *    EDGEONE_TOKEN / EDGEONE_PAGES_API_TOKEN   无头环境部署令牌（本地已登录时可省）
 *    GITHUB_OUTPUT                             存在时自动追加 url / canonical / deployment
 *
 *  退出码：0 成功；1 任一环节失败；2 用法/前置条件错误
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { DESTINATIONS, inspectDestination, syncDestination } from './sync-web-assets.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO = path.resolve(HERE, '..')
const APP_DIR = path.join(REPO, 'deploy', 'edgeone-app')
const WEB_DIR = path.join(REPO, 'email-aggregator-web')
const VERSION_TOOL = path.join(HERE, 'version.mjs')
const BINDING_FILE = path.join(APP_DIR, 'edgeone-project.json')
const EDGEONE_DIR = path.join(APP_DIR, '.edgeone')
const DEPLOY_LOG = path.join(EDGEONE_DIR, 'last-deploy.json')

// ============================================================================
//  参数
// ============================================================================

const argv = process.argv.slice(2)
const HAS = (f) => argv.includes(f)
const valueOf = (f) => {
  const i = argv.indexOf(f)
  return i >= 0 ? argv[i + 1] : undefined
}

const JSON_OUT = HAS('--json')
const DRY_RUN = HAS('--dry-run')
const SKIP_BUILD = HAS('--skip-build')
const SKIP_SMOKE = HAS('--skip-smoke')
const VERIFY_LIVE = HAS('--verify-live')

if (HAS('-h') || HAS('--help')) {
  process.stdout.write(fs.readFileSync(fileURLToPath(import.meta.url), 'utf8').split('*/')[0] + '\n')
  process.exit(0)
}

/** 供 --json 输出与 GITHUB_OUTPUT 使用的结构化结果。 */
const result = {
  ok: false,
  canonical: null,
  semver: null,
  code: null,
  project: null,
  projectId: null,
  deployment: null,
  url: null,
  steps: [],
  liveVerify: null,
}

const log = (...a) => {
  if (!JSON_OUT) console.log(...a)
}
const step = (n, title) => {
  result.steps.push({ n, title, ok: null })
  log('')
  log(`==> [${n}] ${title}`)
}
const done = (extra = '') => {
  result.steps[result.steps.length - 1].ok = true
  log(`    OK${extra ? '  ' + extra : ''}`)
}
const die = (msg, code = 1) => {
  if (JSON_OUT) console.log(JSON.stringify({ ...result, ok: false, error: msg }, null, 2))
  else console.error(`\n[deploy] 失败：${msg}\n`)
  process.exit(code)
}

// ============================================================================
//  进程执行
// ============================================================================

/**
 * 带 shell 时的参数引用。
 *
 * 为什么需要：Windows 下 npm / edgeone 都是 .cmd 包装脚本，Node 出于
 * CVE-2024-27980 的缓解措施不允许直接 spawn .cmd，必须 shell:true；
 * 而 shell:true 时 Node 只是把参数用空格拼起来，不做引用 ——
 * 于是令牌里只要有一个空格或 & 就会把命令拆坏（CI secret 尤甚）。
 * POSIX 下 npm/edgeone 是带 shebang 的普通文件，无需 shell，也就无需引用。
 */
const NEED_SHELL = process.platform === 'win32'

function quoteArgs(args) {
  if (!NEED_SHELL) return args
  return args.map((a) => (/[\s"&|<>^()%!]/.test(a) ? `"${String(a).replace(/"/g, '""')}"` : String(a)))
}

/**
 * 执行子进程。
 * @param {string} bin
 * @param {string[]} args
 * @param {{cwd?:string, capture?:boolean, env?:Record<string,string>, label?:string}} [opts]
 */
function run(bin, args, opts = {}) {
  // shell:true 时 Node 会把「命令 + 参数」直接拼成一行字符串交给 cmd/sh，
  // 因此带空格的解释器路径（如 C:\Program Files\nodejs\node.exe）必须自己加引号，
  // 否则会被拆成两个 token，报 "不是内部或外部命令"。
  const cmd = NEED_SHELL && /\s/.test(bin) ? `"${bin}"` : bin
  const r = spawnSync(cmd, quoteArgs(args), {
    cwd: opts.cwd ?? REPO,
    encoding: 'utf8',
    stdio: opts.capture ? ['ignore', 'pipe', 'pipe'] : 'inherit',
    env: { ...process.env, ...(opts.env ?? {}) },
    shell: NEED_SHELL,
    maxBuffer: 32 * 1024 * 1024,
  })
  if (r.error) die(`无法执行 ${opts.label ?? bin}：${r.error.message}`)
  return { code: r.status ?? 1, stdout: r.stdout ?? '', stderr: r.stderr ?? '' }
}

/** 必须在 PATH 中存在的命令；缺失时给出可直接复制的补全命令。 */
function requireBin(bin, hint, versionRe) {
  const probe = run(bin, ['--version'], { capture: true })
  const text = `${probe.stdout}\n${probe.stderr}`
  // 有些 CLI 的 --version 会先打印 banner（edgeone 就是），故不能直接取首行。
  const m = versionRe ? text.match(versionRe) : null
  if (probe.code !== 0 && !m) {
    die(`未找到命令 \`${bin}\`。${hint ? '\n  安装：' + hint : ''}`, 2)
  }
  return m ? m[1] ?? m[0] : '已就绪'
}

// ============================================================================
//  ① 版本门禁
// ============================================================================

function versionGate() {
  // 先 sync 再 verify：sync 幂等（无变化不触碰磁盘），可顺手修复本地忘记下发的落点；
  // verify 才是真正的门禁 —— 它不写盘，只读，所以能抓住"生成器本身写错"的情况。
  const s = run(process.execPath, [VERSION_TOOL, 'sync'], { capture: true })
  if (s.code !== 0) die(`版本下发失败：\n${s.stdout}${s.stderr}`)

  const v = run(process.execPath, [VERSION_TOOL, 'verify'], { capture: true })
  if (v.code !== 0) die(`版本落点不一致，已中止发布：\n${v.stdout}${v.stderr}`)

  // 落点总数从注册表实时读取，不写死数字 —— 写死的那种注释会在新增落点后变成谎言。
  const l = run(process.execPath, [VERSION_TOOL, 'list', '--json'], { capture: true })
  if (l.code !== 0) die('读取落点注册表失败')
  const reg = JSON.parse(l.stdout)
  const info = reg.version
  const skipped = reg.targets.filter((t) => t.status === 'skip').length
  Object.assign(result, {
    canonical: info.canonical,
    semver: info.semver,
    code: info.code,
  })
  log(`    版本 ${info.canonical}（semver=${info.semver}, code=${info.code}）`)
  log(
    `    版本落点 ${reg.targets.length} 个全部一致` +
      (skipped ? `（跳过 ${skipped} 个未生成脚手架的落点）` : '') +
      `；产物同步目标登记 ${DESTINATIONS.length} 处`,
  )
}

// ============================================================================
//  ② 前端构建
// ============================================================================

function buildFrontend() {
  if (SKIP_BUILD) {
    log('    跳过（--skip-build）：复用现有 email-aggregator-web/dist')
  } else {
    if (!fs.existsSync(path.join(WEB_DIR, 'node_modules'))) {
      log('    node_modules 缺失，先安装依赖')
      const i = run('npm', ['ci'], { cwd: WEB_DIR })
      if (i.code !== 0) {
        log('    npm ci 失败，回退 npm install')
        const i2 = run('npm', ['install'], { cwd: WEB_DIR })
        if (i2.code !== 0) die('前端依赖安装失败')
      }
    }
    const b = run('npm', ['run', 'build'], { cwd: WEB_DIR })
    if (b.code !== 0) die('前端构建失败（tsc --noEmit && vite build）')
  }

  const dist = path.join(WEB_DIR, 'dist')
  if (!fs.existsSync(path.join(dist, 'index.html'))) {
    die(`前端产物不存在：${path.relative(REPO, dist)}/index.html\n  提示：去掉 --skip-build 以先执行构建`)
  }
  const assets = path.join(dist, 'assets')
  const n = fs.existsSync(assets) ? fs.readdirSync(assets).length : 0
  log(`    dist/index.html + assets/（${n} 个文件）就绪`)
}

// ============================================================================
//  ③ 产物同步（五处目标）
// ============================================================================

function syncAssets() {
  for (const d of DESTINATIONS) {
    const r = syncDestination(d)
    if (r.action === 'skip') {
      log(`    跳过   ${d.dir}  （${d.label}）`)
    } else {
      log(`    已同步 ${d.dir}  拷贝 ${r.copied}  清理 ${r.removed}`)
    }
  }

  // 复验：同步后必须逐文件 SHA-256 一致。这一步是"漏同步"唯一的机器可判定证据。
  const rows = DESTINATIONS.map(inspectDestination)
  const bad = rows.filter((r) => r.status === 'drift')
  for (const r of bad) {
    log(`    DRIFT  ${r.dir}`)
    for (const f of r.missing.slice(0, 8)) log(`           · 缺失 ${f}`)
    for (const f of r.diff.slice(0, 8)) log(`           · 内容不同 ${f}`)
  }
  if (bad.length) die(`产物同步后复验仍不一致：${bad.length} 处`)
  log(`    复验：${DESTINATIONS.length} 处目标一致 OK`)
}

// ============================================================================
//  ④ 项目绑定
// ============================================================================

function restoreBinding(projectName) {
  if (!fs.existsSync(BINDING_FILE)) {
    die(`缺少项目绑定文件：${path.relative(REPO, BINDING_FILE)}`, 2)
  }
  let binding
  try {
    binding = JSON.parse(fs.readFileSync(BINDING_FILE, 'utf8'))
  } catch (e) {
    die(`项目绑定文件不是合法 JSON：${e.message}`, 2)
  }
  if (!binding.Name || !binding.ProjectId) {
    die('项目绑定文件必须同时含 Name 与 ProjectId', 2)
  }
  if (projectName && binding.Name !== projectName) {
    die(
      `项目名不一致：-n 指定 "${projectName}"，绑定文件写着 "${binding.Name}"。\n` +
        '  CLI 按项目名远程解析，两者不同名会**另建一个新项目**（旧项目与域名都会失联）。\n' +
        '  请统一 edgeone-project.json 与 --project 后再部署。',
      2,
    )
  }

  // .edgeone/ 被 gitignore（含平台产物与本地状态），故每次部署都要从入库的
  // edgeone-project.json 重新播种，保证「绑定文件」与「命令行 -n」永远同源。
  fs.mkdirSync(EDGEONE_DIR, { recursive: true })
  fs.writeFileSync(
    path.join(EDGEONE_DIR, 'project.json'),
    JSON.stringify({ Name: binding.Name, ProjectId: binding.ProjectId }),
    'utf8',
  )
  Object.assign(result, { project: binding.Name, projectId: binding.ProjectId })
  log(`    项目 ${binding.Name} / ProjectId ${binding.ProjectId}（已写入 .edgeone/project.json）`)
}

// ============================================================================
//  ⑤ 冒烟 + 部署
// ============================================================================

function smoke() {
  if (SKIP_SMOKE) {
    log('    跳过（--skip-smoke）—— 不推荐，冒烟是部署前唯一能拦住路由级回归的门禁')
    return
  }
  for (const [label, args] of [
    ['演示形态', ['smoke.mjs']],
    ['生产形态', ['smoke.mjs', '--prod']],
  ]) {
    const r = run(process.execPath, args, { cwd: APP_DIR, capture: true })
    const tail = (r.stdout.trim().split('\n').pop() ?? '').trim()
    if (r.code !== 0) {
      die(`冒烟测试未通过（${label}）：\n${r.stdout}${r.stderr}`)
    }
    log(`    冒烟 ${label}：${tail}`)
  }
}

function parseJsonLoose(text) {
  const t = String(text).trim()
  try {
    return JSON.parse(t)
  } catch {
    /* 继续尝试从整段输出里抠出 JSON 对象 */
  }
  const i = t.indexOf('{')
  const j = t.lastIndexOf('}')
  if (i >= 0 && j > i) {
    try {
      return JSON.parse(t.slice(i, j + 1))
    } catch {
      return null
    }
  }
  return null
}

function deepFind(node, pred, depth = 0) {
  if (node == null || depth > 8) return undefined
  if (typeof node === 'string' || typeof node === 'number') return undefined
  if (typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) {
      if (typeof v === 'string' && pred(k, v)) return v
      const r = deepFind(v, pred, depth + 1)
      if (r !== undefined) return r
    }
  }
  return undefined
}

function deploy(projectName) {
  const token = process.env.EDGEONE_TOKEN || process.env.EDGEONE_PAGES_API_TOKEN || ''
  const args = ['makers', 'deploy', '-n', projectName]
  if (token) args.push('-t', token)
  args.push('--json')

  const cli = requireBin('edgeone', 'npm install -g edgeone@latest')
  log(`    edgeone CLI ${cli}${token ? '（使用令牌）' : '（使用本地登录态）'}`)

  if (DRY_RUN) {
    log('    --dry-run：已跳过真实部署')
    return
  }

  // PAGES_SOURCE=skills 是平台侧约定变量（CLI 据此选择 Makers 部署通道）。
  const r = run('edgeone', args, { cwd: APP_DIR, capture: true, env: { PAGES_SOURCE: 'skills' } })
  if (r.code !== 0) {
    die(`部署失败（exit ${r.code}）：\n${r.stdout}\n${r.stderr}`)
  }

  const parsed = parseJsonLoose(r.stdout)
  const url = parsed
    ? deepFind(parsed, (_k, v) => /^https?:\/\//.test(v) && v.includes('eo_token'))
    : undefined
  const dep = parsed
    ? deepFind(parsed, (k, v) => typeof v === 'string' && /^dep/i.test(k))
    : undefined

  Object.assign(result, { deployment: dep ?? null, url: url ?? null })

  fs.mkdirSync(EDGEONE_DIR, { recursive: true })
  fs.writeFileSync(
    DEPLOY_LOG,
    JSON.stringify(
      {
        at: new Date().toISOString(),
        canonical: result.canonical,
        project: projectName,
        projectId: result.projectId,
        deployment: result.deployment,
        url: result.url,
        raw: parsed ?? r.stdout.slice(0, 4000),
      },
      null,
      2,
    ),
    'utf8',
  )

  log(`    部署 ID ${result.deployment ?? '（CLI 未返回）'}`)
  if (result.url) {
    log('')
    log('    ⏱  预览链接约 30 分钟后失效（带 eo_token）。再次验证前请重新部署取新 token。')
    log(`    ${result.url}`)
  } else {
    log('    未从 CLI 输出中解析出带 eo_token 的 URL，原始输出已存 .edgeone/last-deploy.json')
  }
  log(`    部署记录已写入 ${path.relative(REPO, DEPLOY_LOG)}`)
}

// ============================================================================
//  ⑥ 线上验证（curl 驱动，绕开预览网关的三个坑）
// ============================================================================

function verifyLive() {
  if (!result.url) {
    log('    跳过：没有可用的预览 URL')
    return
  }
  const script = path.join(REPO, 'scripts', 'live-verify.mjs')
  if (!fs.existsSync(script)) {
    log(`    跳过：缺少 ${path.relative(REPO, script)}`)
    return
  }
  // 用 Node 实现而非 shots/live-verify.sh：
  //   ⒈ 本机 bash 是残缺的 PortableGit shim，`bash --version` 都返回 1，根本起不来；
  //   ⒉ Windows/CI 一致性——Node 是这套工具链唯一无条件可用的运行时；
  //   ⒊ 需要 curl 才能拿到 HttpOnly 的 eo_token（Node fetch 不带 cookie jar），
  //      实现内部固定调 curl.exe，见 scripts/live-verify.mjs 头注释。
  // 判定以**退出码**为准（脚本自身会 exit 1），结果行仅用于展示与留档。
  log('    执行 node scripts/live-verify.mjs "<url>"')
  const r = run(process.execPath, ['scripts/live-verify.mjs', result.url], { cwd: REPO, capture: true })
  const lines = (r.stdout + '\n' + r.stderr).split('\n').filter((l) => l.trim())
  for (const l of lines) if (/^(PASS|FAIL|结果|失败项|      )/.test(l)) log('    ' + l.trim())

  const summary = [...lines].reverse().find((l) => /结果（线上全接口）/.test(l)) ?? ''
  const m = summary.match(/(\d+)\s*通过\s*\/\s*(\d+)\s*失败/)
  result.liveVerify = m ? { pass: +m[1], fail: +m[2] } : { pass: null, fail: null, raw: summary }
  if (!m) die('线上验证未产出可判定的结果行（可能未取得预览会话 cookie，请确认 token 未过期）')
  if (r.code !== 0 || +m[2] > 0) die(`线上验证存在 ${m[2]} 项失败（见上方 FAIL 行）`)
  log(`    线上验证通过：${m[1]} 项全绿`)
}

// ============================================================================
//  主流程
// ============================================================================

function writeGitHubOutput() {
  const file = process.env.GITHUB_OUTPUT
  if (!file) return
  const kv = {
    canonical: result.canonical ?? '',
    semver: result.semver ?? '',
    code: result.code ?? '',
    project: result.project ?? '',
    deployment: result.deployment ?? '',
    url: result.url ?? '',
  }
  try {
    fs.appendFileSync(file, Object.entries(kv).map(([k, v]) => `${k}=${v}`).join('\n') + '\n', 'utf8')
  } catch {
    /* GITHUB_OUTPUT 不可写不应导致发布失败 */
  }
}

function main() {
  log('项目003「邮箱聚合平台」· 官网发布（EdgeOne Makers）')
  log('─'.repeat(60))

  step(1, '版本门禁（version.json → 全部版本落点）')
  versionGate()
  done(result.canonical)

  step(2, '前端构建（email-aggregator-web）')
  buildFrontend()
  done()

  step(3, '产物同步（五处目标 + SHA-256 复验）')
  syncAssets()
  done()

  step(4, '项目绑定（防止 CLI 另建项目）')
  restoreBinding(valueOf('--project'))
  done()

  step(5, '本地冒烟 + 部署')
  smoke()
  deploy(result.project)
  done(DRY_RUN ? '（dry-run）' : result.deployment ?? '')

  if (VERIFY_LIVE && !DRY_RUN) {
    step(6, '线上全接口验证')
    verifyLive()
    done()
  }

  result.ok = true
  writeGitHubOutput()

  if (JSON_OUT) {
    console.log(JSON.stringify(result, null, 2))
    return
  }
  log('')
  log('─'.repeat(60))
  log(`发布完成：${result.canonical}  →  项目 ${result.project}（${result.projectId}）`)
  if (DRY_RUN) log('（--dry-run：未真正部署）')
}

main()
