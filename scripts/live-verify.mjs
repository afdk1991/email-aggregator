#!/usr/bin/env node
/**
 * ============================================================================
 *  项目003「邮箱聚合平台」· 线上全接口验证（单一实现）
 * ============================================================================
 *
 *  为什么从 bash 移植到 Node：
 *    原本只有 shots/live-verify.sh 一份实现，而它需要可执行的 bash + sed/grep/tail/
 *    dirname/date/wc。本机（Windows）的 PortableGit shim 已残缺到 `bash --version`
 *    都返回 "The system cannot execute the specified program" —— 于是**本地永远无法
 *    验证线上**，只能靠 CI（Ubuntu）。这既是能力缺口，也与"一件事一份实现"的
 *    收敛原则冲突：现在 Node 版是唯一实现，live-verify.sh 退化为薄壳转发。
 *
 *  为什么仍然由 curl 驱动、而不是 Node 的 fetch：
 *    EdgeOne 预览网关把 eo_token 下发为 **HttpOnly cookie**，Node 的 fetch 拿不到
 *    HttpOnly 项（`getSetCookie` 在部分实现中不返回）→ 后续请求全部 401。
 *    curl 的 cookie jar 能正确处理，故 Node 只负责"逻辑与断言"，网络交给 curl。
 *    cookie jar 固定落在 os.tmpdir()（ASCII 路径）—— 实测 curl 无法把 jar 写到
 *    含中文的绝对路径，表现为 jar 静默不生成、后续全 401。
 *
 *  三条必须遵守的预览网关约定（违反会得到一片假的 401/400）：
 *    1. 建立会话：唯一一次带 query 的请求，换取 eo_token / eo_time 两个 cookie。
 *    2. 会话建立后**一律不再带 query** —— 带 `?eo_token=` 的 POST 会让云函数拿不到
 *       request body，接口回 400 "id and provider are required"。
 *    3. path 必须拼在 query 之前（本实现按 ORIGIN + path 拼，天然满足）。
 *
 *  用法：
 *    node scripts/live-verify.mjs "<带 eo_token 的完整 URL>"
 *    node scripts/live-verify.mjs "<URL>" --json        机器可读输出
 *    node scripts/live-verify.mjs "<URL>" --timeout 45  单请求超时（秒，默认 30）
 *
 *  退出码：0 全部通过；1 存在失败项；2 用法/前置条件错误（如无 curl、未取得会话）
 *
 *  ⏱️ 预览 token 约 30 分钟失效；老 token 表现为全 401 且 jar 里没有 eo_token。
 * ============================================================================
 */

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'

const argv = process.argv.slice(2)
const JSON_OUT = argv.includes('--json')
const positional = argv.filter((a) => !a.startsWith('--'))
const tIdx = argv.indexOf('--timeout')
const TIMEOUT = tIdx >= 0 ? String(Number(argv[tIdx + 1]) || 30) : '30'

const RAW = positional[0]
if (!RAW) {
  console.error('用法：node scripts/live-verify.mjs "<带 eo_token 的完整 URL>" [--json] [--timeout 30]')
  process.exit(2)
}

const qIdx = RAW.indexOf('?')
const ORIGIN = qIdx >= 0 ? RAW.slice(0, qIdx) : RAW
const QUERY = qIdx >= 0 ? RAW.slice(qIdx + 1) : ''

const JAR = path.join(os.tmpdir(), 'p003-live-verify-cookies.txt')
const WARM = path.join(os.tmpdir(), 'p003-live-verify-warmup.txt')

// ============================================================================
//  curl 封装
// ============================================================================

/** 直接 spawn curl，**不经 shell**：URL / JSON body 里的 & ? " 不会被 cmd 拆解。 */
function curl(args) {
  const r = spawnSync('curl', args, { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 })
  if (r.error) {
    console.error(`[live-verify] 无法执行 curl：${r.error.message}`)
    console.error('  Windows 10 1803+ 自带 curl.exe；若缺失请安装 curl 并加入 PATH。')
    process.exit(2)
  }
  return { code: r.status ?? 1, stdout: r.stdout ?? '', stderr: r.stderr ?? '' }
}

/** 带状态码的请求：返回 { status, body }。 */
function request(pathname, { method = 'GET', body } = {}) {
  const url = `${ORIGIN}${pathname}`
  const args = [
    '-sL',
    '--max-time', TIMEOUT,
    '-c', JAR,
    '-b', JAR,
    '-w', '\n%{http_code}',
  ]
  if (method !== 'GET') {
    args.push('-X', method)
    if (body !== undefined) {
      args.push('-H', 'content-type: application/json', '--data-binary', body)
    }
  }
  args.push(url)
  const r = curl(args)
  const nl = r.stdout.lastIndexOf('\n')
  const code = Number((nl >= 0 ? r.stdout.slice(nl + 1) : r.stdout).trim()) || 0
  const text = nl >= 0 ? r.stdout.slice(0, nl) : ''
  return { status: code, body: text }
}

const getJSON = (pathname, opts) => {
  const { status, body } = request(pathname, opts)
  try {
    return { status, data: JSON.parse(body) }
  } catch {
    return { status, data: { __raw: body.slice(0, 160) } }
  }
}

// ============================================================================
//  断言
// ============================================================================

let pass = 0
const failures = []

function check(name, ok, extra = '') {
  if (ok) {
    pass++
    if (!JSON_OUT) console.log(`PASS  ${name}`)
  } else {
    failures.push(name)
    if (!JSON_OUT) console.log(`FAIL  ${name}${extra ? ' → ' + extra : ''}`)
  }
}

const log = (...a) => {
  if (!JSON_OUT) console.log(...a)
}

// ============================================================================
//  主流程
// ============================================================================

function establishSession() {
  fs.rmSync(JAR, { force: true })
  const r = curl([
    '-sL', '--max-time', TIMEOUT,
    '-c', JAR, '-b', JAR,
    `${ORIGIN}/?${QUERY}`,
    '-o', WARM,
  ])
  const jar = fs.existsSync(JAR) ? fs.readFileSync(JAR, 'utf8') : ''
  const hasToken = jar.includes('eo_token')
  if (!hasToken) {
    // 未取得会话是**前置条件失败**，不是断言失败：继续跑只会得到一片假的 401。
    const hint =
      r.stderr.trim() ||
      (QUERY ? 'cookie jar 中没有 eo_token' : 'URL 未携带 query（eo_token 参数）')
    console.error(`[live-verify] 未能建立预览会话，已中止。\n  目标：${ORIGIN}\n  原因：${hint}`)
    console.error('  最常见原因：eo_token 已过期（预览链接约 30 分钟有效）→ 重新部署取新链接。')
    process.exit(2)
  }
}

function main() {
  log(`=== 目标：${ORIGIN}`)
  establishSession()
  log('    预览会话已建立（eo_token / eo_time 已落入 cookie jar）')
  log('')

  // ---------------- 1. 静态资源 ----------------
  const root = request('/')
  check('GET / 返回 200', root.status === 200, `实际 ${root.status}`)

  const jsMatch = root.body.match(/\/assets\/index-[A-Za-z0-9_-]*\.js/)
  const cssMatch = root.body.match(/\/assets\/index-[A-Za-z0-9_-]*\.css/)
  const JS_PATH = jsMatch ? jsMatch[0] : ''
  const CSS_PATH = cssMatch ? cssMatch[0] : ''
  check('HTML 引用 JS 产物', !!JS_PATH, JS_PATH)
  check('HTML 引用 CSS 产物', !!CSS_PATH, CSS_PATH)
  log(`      JS=${JS_PATH || '(未找到)'}  CSS=${CSS_PATH || '(未找到)'}`)

  if (JS_PATH) {
    const js = request(JS_PATH)
    check('JS 产物可获取', js.status === 200, `实际 ${js.status}`)
    check('JS 产物体量正常(>10KB)', js.body.length > 10000, `长度 ${js.body.length}`)
    for (const kw of ['模拟收信', 'demo-tenant', 'demo/push']) {
      check(`生产包不含 '${kw}'`, !js.body.includes(kw), '命中演示关键字')
    }
  }

  if (CSS_PATH) {
    const css = request(CSS_PATH)
    check('CSS 产物可获取', css.status === 200, `实际 ${css.status}`)
    log(`      CSS 长度 ${css.body.length}`)
    check('无障碍修正已上线(--muted #667085)', css.body.includes('#667085'))
    check('badge 高度 24px(WCAG 2.5.8)', css.body.includes('height:24px'))
    check('含 @supports 兼容兜底', css.body.includes('@supports'))
    check('含暗色主题', css.body.includes('data-theme=dark') || css.body.includes("data-theme='dark'"))
    check('含布局令牌', css.body.includes('--sidebar-w'))
  }

  log('')

  // ---------------- 2. health ----------------
  const hRaw = request('/api/health')
  check('GET /api/health → 200', hRaw.status === 200, `实际 ${hRaw.status}`)
  const h = hRaw.body
  check('health.ok === true', h.includes('"ok":true'))
  check('Blob 强一致持久化生效', h.includes('"mode":"serverless-blob"'), h.slice(0, 200))
  check('生产形态 demo=false', h.includes('"demo":false'))
  log(`      ${h.slice(0, 200)}`)

  log('')

  // ---------------- 3. 账户 CRUD ----------------
  const tid = `acc_verify_${Date.now()}`
  const created = getJSON('/api/accounts', {
    method: 'POST',
    body: JSON.stringify({ id: tid, provider: 'imap', email: `${tid}@example.com`, displayName: '验证账户' }),
  })
  check('POST /api/accounts → 201', created.status === 201, `实际 ${created.status}`)

  const list = request('/api/accounts')
  check('GET /api/accounts → 200', list.status === 200, `实际 ${list.status}`)
  check('新建账户出现在列表', list.body.includes(tid))

  const upd = getJSON(`/api/accounts/${tid}`, { method: 'PUT', body: JSON.stringify({ displayName: '验证账户-已改' }) })
  check('PUT 部分更新 → 200', upd.status === 200, `实际 ${upd.status}`)
  check('PUT 保留 provider 字段', JSON.stringify(upd.data).includes('"provider":"imap"'))

  const del = request(`/api/accounts/${tid}`, { method: 'DELETE' })
  check('DELETE 账户 → 200', del.status === 200, `实际 ${del.status}`)

  const after = request('/api/accounts')
  check('删除后账户不可见', !after.body.includes(tid), '账户仍存在')

  log('')

  // ---------------- 4. 退役演示账户防护 ----------------
  const ban = getJSON('/api/accounts', {
    method: 'POST',
    body: JSON.stringify({ id: 'acc_demo3', provider: 'imap', email: 'x@example.com' }),
  })
  check('重建已下线演示账户被拒绝', ban.status >= 400, `实际 ${ban.status}`)

  const fin = request('/api/accounts')
  const retired = ['acc_demo3', 'acc_demo32', 'acc_personal1', 'acc_personal11', 'acc_work1', 'acc_work11']
  const found = retired.filter((id) => fin.body.includes(id))
  check('线上无任何退役演示账户', found.length === 0, `发现 ${found.join(', ')}`)

  log('')

  // ---------------- 5. 检索 ----------------
  const search = request('/api/search?q=test')
  check('GET /api/search → 200', search.status === 200, `实际 ${search.status}`)
  check('检索返回结果集合', /"hits"|"results"/.test(search.body))

  log('')

  // ---------------- 6. 生产形态演示接口必须关闭 ----------------
  const push = request('/api/demo/push', { method: 'POST' })
  check('生产形态 /api/demo/push → 404', push.status === 404, `实际 ${push.status}`)

  log('')

  // ---------------- 7. AI 降级行为 ----------------
  const ai = getJSON('/api/ai/chat', {
    method: 'POST',
    body: JSON.stringify({ messages: [{ role: 'user', content: '你好' }] }),
  })
  check('POST /api/ai/chat 未发生未捕获崩溃(非 500)', ai.status !== 500, `实际 ${ai.status}`)
  check('AI 不伪造模型输出', !String(ai.data.content || '').includes('[demo]'), '检测到 [demo] 伪造回复')
  log(`      AI 状态 ${ai.status}：${String(ai.data.content || JSON.stringify(ai.data)).slice(0, 110)}`)

  log('')

  // ---------------- 8. 错误处理 ----------------
  const nope = request('/api/__no_such_path__')
  check('未知 /api 路径 → 404', nope.status === 404, `实际 ${nope.status}`)

  log('')

  // ---------------- 9. 邮件列表 ----------------
  const mails = request('/api/mails')
  check('GET /api/mails → 200', mails.status === 200, `实际 ${mails.status}`)
  check('邮件列表结构正确', /"mails"|"items"/.test(mails.body))

  log('')

  // ---------------- 10. SPA 深链回退 ----------------
  const deep = request('/some/deep/link')
  check('SPA 深链回退到 index', deep.status === 200, `实际 ${deep.status}`)

  fs.rmSync(JAR, { force: true })
  fs.rmSync(WARM, { force: true })

  const total = pass + failures.length
  if (JSON_OUT) {
    console.log(
      JSON.stringify({ origin: ORIGIN, ok: failures.length === 0, pass, fail: failures.length, total, failures }, null, 2),
    )
  } else {
    log('')
    log(`结果（线上全接口）：${pass} 通过 / ${failures.length} 失败`)
    if (failures.length) log(`失败项：\n  - ${failures.join('\n  - ')}`)
  }
  process.exit(failures.length === 0 ? 0 : 1)
}

main()
