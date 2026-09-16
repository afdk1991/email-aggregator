// 邮箱聚合平台 · EdgeOne Makers 云函数入口（Node.js Handler 模式）
//
// 路由：文件 api/[[default]].js → 线上 /api/*（catch-all）。
//
// 为什么用 Node 而不是 Go：Makers 的 Go 构建器在 Windows 下会生成无法编译的 main.go
// （实测报错 main.go:51:19 unknown escape，与本项目代码无关——极简 10 行 handler 同样报错），
// 而 Node 运行时在平台上支持成熟。
//
// 契约：路径、字段、状态码与 email-aggregator-go/src/api/server.go 一致，前端零改动。
//
// 存储：Blob（@edgeone/pages-blob），平台无托管数据库，Blob 即数据库。
//   state.json —— 单文档保存 { accounts, mails }，一次读、一次写。
//   选单文档而非分键：数据量小（数十条），读改写天然一致，
//   彻底避免"实例 A 删除、实例 B 仍返回旧缓存"导致删除看起来不生效。
//   每个请求都从 Blob 读取，不做跨请求内存缓存 —— Serverless 多实例无法互相通知。
//
// 演示账户清理：BANNED_IDS 中的账户及其邮件在读取时被剔除并写回，
// 且种子不再生成它们，保证删除后不会被重新播种。

const DEFAULT_ACCOUNT = 'acc_demo'
const APP_VERSION = 'MALLV0.0.0'
const APP_VERSION_CODE = 0
const STORE_NAME = 'mailbox'
// v2：演示数据清理后启用新键，等同于把 Blob 里旧种子数据整体作废（无需手工删库）。
const STATE_KEY = 'state.v2.json'

// 已下线的演示账户：读时被剔除，创建时被拒绝
const BANNED_IDS = [
  'acc_demo3',
  'acc_demo32',
  'acc_personal1',
  'acc_personal11',
  'acc_work1',
  'acc_work11',
]

// 演示模式开关（ENABLE_DEMO=false 即生产形态）：
//   true  → 预置种子邮件、开放 /api/demo/push、AI 兜底演示回复
//   false → 空库起步、演示路由 404、AI 无上游时返回 503 而非伪造答案
// 缺省为 true 以兼容本地联调；部署时必须显式置 false。
let demoEnabled = true

// ------------------------------------------------------------------ Blob 存储

let storage // undefined=未探测, null=不可用, object=已就绪
let state = null
let mode = 'memory'

async function blob() {
  if (storage !== undefined) return storage
  try {
    const mod = await import('@edgeone/pages-blob')
    storage = mod.getStore({ name: STORE_NAME, consistency: 'strong' })
  } catch {
    storage = null
  }
  return storage
}

const docToState = (doc) => ({
  accounts: new Map(Object.entries(doc.accounts || {})),
  mails: new Map(Object.entries(doc.mails || {})),
})

const stateToDoc = (st) => ({
  accounts: Object.fromEntries(st.accounts),
  mails: Object.fromEntries(st.mails),
})

// 剔除已下线演示账户及其邮件；返回是否有改动
function purgeBanned(st) {
  let changed = false
  for (const id of BANNED_IDS) {
    if (st.accounts.delete(id)) changed = true
  }
  for (const [k, m] of [...st.mails]) {
    if (BANNED_IDS.includes(m.accountId)) {
      st.mails.delete(k)
      changed = true
    }
  }
  return changed
}

async function persist() {
  const s = await blob()
  if (!s || !state) return
  try {
    await s.setJSON(STATE_KEY, stateToDoc(state))
  } catch {
    /* 写入失败不阻断响应 */
  }
}

async function ensure() {
  const s = await blob()
  if (s) {
    try {
      const doc = await s.get(STATE_KEY, { type: 'json' })
      if (doc && (doc.accounts || doc.mails)) {
        state = docToState(doc)
        if (purgeBanned(state)) await persist()
        mode = 'blob'
        return state
      }
    } catch {
      /* 读取失败则重新播种 */
    }
    state = initState()
    await persist()
    mode = 'blob'
    return state
  }
  if (!state) state = initState()
  mode = 'memory'
  return state
}

// ------------------------------------------------------------------ 演示种子

// 生产形态（ENABLE_DEMO=false）：不预置任何种子数据，空库起步，
// 邮件只可能来自真实同步流水线或用户自己的写入。
function initState() {
  const now = Math.floor(Date.now() / 1000)
  const mails = new Map()
  const accounts = new Map()
  if (!demoEnabled) return { mails, accounts, now }

  const addMail = (m) => {
    m.snippet = m.bodyText.length > 80 ? m.bodyText.slice(0, 80) : m.bodyText
    mails.set(m.id, m)
  }

  const mail = (id, accountId, provider, fromName, fromEmail, to, subject, bodyText,
    offsetSec, sizeBytes, hasAttachment, read, lastUid) =>
    addMail({
      id,
      accountId,
      provider,
      folder: 'INBOX',
      from: { name: fromName, email: fromEmail },
      to: [{ name: '', email: to }],
      cc: [],
      bcc: [],
      subject,
      bodyText,
      hasAttachment,
      attachments: [],
      internalDate: now - offsetSec,
      sizeBytes,
      cursor: { lastUid },
      read,
    })

  mail('m1', 'acc_demo', 'imap', 'Alice', 'alice@example.com', 'me@example.com',
    'Welcome onboard', 'Welcome to the platform, your account is ready.', 7200, 1024, false, false, 1)
  mail('m2', 'acc_demo', 'imap', 'Bob', 'bob@vendor.com', 'me@example.com',
    'Invoice #2024-03', 'Please find attached the invoice for March services.', 3600, 2048, true, false, 2)
  mail('m3', 'acc_demo', 'imap', 'Carol', 'carol@meet.com', 'me@example.com',
    'Team sync meeting', 'Reminder: weekly team sync at 10am tomorrow.', 1800, 1536, false, false, 3)
  mail('w1', 'acc_work', 'gmail', '项目 PM', 'pm@corp.com', 'me@corp.com',
    'Sprint 复盘纪要', '本周 Sprint 已收尾，复盘结论：接口契约对齐完成，下周进入联调。', 5400, 1800, false, false, 1)
  mail('w2', 'acc_work', 'gmail', '法务', 'legal@corp.com', 'me@corp.com',
    '客户 A 合同待签署', '客户 A 的年度框架合同已生成，请在周五前完成电子签署。', 2700, 2200, true, true, 2)
  mail('p1', 'acc_personal', 'imap', '家人', 'family@home.com', 'me@home.com',
    '周末家庭聚餐邀约', '这周六老家聚餐，记得提前安排时间，爸妈准备了你爱吃的菜。', 9000, 900, false, false, 1)
  mail('p2', 'acc_personal', 'imap', '账单中心', 'billing@utility.com', 'me@home.com',
    '水电费账单已出', '本月水电气账单已生成，合计 ¥328.50，请于月底前缴费。', 8600, 760, false, true, 2)

  const account = (id, provider, email, displayName) => ({
    id,
    unread: 0,
    provider,
    email,
    displayName,
    status: 'active',
    syncFolder: 'INBOX',
    serverHost: '',
    lastSyncAt: now,
    syncing: false,
  })
  accounts.set('acc_demo', account('acc_demo', 'imap', 'me@example.com', '演示邮箱'))
  accounts.set('acc_work', account('acc_work', 'gmail', 'me@corp.com', '工作邮箱'))
  accounts.set('acc_personal', account('acc_personal', 'imap', 'me@home.com', '个人邮箱'))

  return { mails, accounts, now }
}

// ------------------------------------------------------------------ 工具

const json = (data, status = 200) =>
  new Response(JSON.stringify(data), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
  })

const err = (msg, status = 400) => json({ error: msg }, status)

function countUnread(accountId) {
  let n = 0
  for (const m of state.mails.values()) {
    if (m.accountId === accountId && !m.read) n++
  }
  return n
}

// 脱敏（ADR-010 Guardrail）：第三方路径出网前抹掉邮箱与长数字串
function redact(text) {
  const before = text
  const out = text
    .replace(/[\w.+-]+@[\w-]+\.[\w.]+/g, '[REDACTED]')
    .replace(/\b1[3-9]\d{9}\b/g, '[REDACTED]')
    .replace(/\b\d{16,19}\b/g, '[REDACTED]')
  return { text: out, changed: out !== before }
}

async function readJSON(request) {
  try {
    return await request.json()
  } catch {
    return {}
  }
}

// ------------------------------------------------------------------ 处理器

async function handleMails(request, url) {
  if (request.method !== 'GET') return err('method not allowed', 405)
  const accountId = url.searchParams.get('accountId') || DEFAULT_ACCOUNT
  const folder = url.searchParams.get('folder') || 'INBOX'
  const limit = parseInt(url.searchParams.get('limit') || '50', 10) || 50
  const list = [...state.mails.values()]
    .filter((m) => m.accountId === accountId && (folder === '' || m.folder === folder))
    .sort((a, b) => b.internalDate - a.internalDate)
    .slice(0, limit)
  return json({ accountId, count: list.length, mails: list })
}

async function handleMailByID(request, id) {
  const m = state.mails.get(id)
  if (!m) return err('mail not found', 404)
  if (request.method === 'GET') return json(m)
  if (request.method === 'DELETE') {
    state.mails.delete(id)
    await persist()
    return json({ ok: true, id })
  }
  return err('method not allowed', 405)
}

async function handleSetRead(request, url, id) {
  if (request.method !== 'POST') return err('method not allowed', 405)
  const m = state.mails.get(id)
  if (!m) return err('mail not found', 404)
  const body = await readJSON(request)
  let read = body.read === true
  const qp = url.searchParams.get('read')
  if (qp !== null) read = qp === 'true' || qp === '1'
  m.read = read
  await persist()
  return json(m)
}

async function handleSearch(request, url) {
  if (request.method !== 'GET') return err('method not allowed', 405)
  const accountId = url.searchParams.get('accountId') || DEFAULT_ACCOUNT
  const query = (url.searchParams.get('q') || '').trim()
  const hits = []
  if (query) {
    const lower = query.toLowerCase()
    for (const m of state.mails.values()) {
      if (m.accountId !== accountId) continue
      const hay = `${m.subject} ${m.bodyText} ${m.from.email}`.toLowerCase()
      if (hay.includes(lower)) {
        hits.push({
          idempotencyKey: `${m.accountId}|${m.folder}|${m.id}`,
          accountId: m.accountId,
          subject: m.subject,
          from: m.from.email,
          preview: m.snippet,
          internalDate: m.internalDate,
        })
      }
    }
    hits.sort((a, b) => b.internalDate - a.internalDate)
  }
  return json({ accountId, query, hits })
}

async function handleAccounts(request) {
  if (request.method === 'GET') {
    const list = [...state.accounts.values()].map((a) => ({ ...a, unread: countUnread(a.id) }))
    list.sort((a, b) => a.id.localeCompare(b.id))
    return json({ accounts: list })
  }
  if (request.method === 'POST') {
    const body = await readJSON(request)
    if (!body.id || !body.provider) return err('id and provider are required', 400)
    if (BANNED_IDS.includes(body.id)) return err('account id is retired', 400)
    if (state.accounts.has(body.id)) return err('account already exists', 409)
    const acc = {
      id: body.id,
      unread: 0,
      provider: body.provider,
      email: body.email || '',
      displayName: body.displayName || '',
      status: body.status || 'active',
      syncFolder: body.syncFolder || 'INBOX',
      serverHost: body.serverHost || '',
      lastSyncAt: Math.floor(Date.now() / 1000),
      syncing: false,
    }
    state.accounts.set(acc.id, acc)
    await persist()
    return json(acc, 201)
  }
  return err('method not allowed', 405)
}

async function handleAccountByID(request, id) {
  const cur = state.accounts.get(id)
  if (!cur) return err('account not found', 404)
  if (request.method === 'PUT') {
    const body = await readJSON(request)
    const merged = { ...cur, ...body, id } // 部分更新：以路径 ID 为准
    state.accounts.set(id, merged)
    await persist()
    return json(merged)
  }
  if (request.method === 'DELETE') {
    state.accounts.delete(id)
    let removed = 0
    for (const [k, m] of [...state.mails]) {
      if (m.accountId === id) {
        state.mails.delete(k)
        removed++
      }
    }
    await persist()
    return json({ ok: true, id, mailsRemoved: removed })
  }
  return err('method not allowed', 405)
}

async function handleAccountSync(request, id) {
  if (request.method !== 'POST') return err('method not allowed', 405)
  const a = state.accounts.get(id)
  if (!a) return err('account not found', 404)
  a.lastSyncAt = Math.floor(Date.now() / 1000)
  a.lastSyncError = demoEnabled
    ? '演示形态：Serverless 无长驻同步进程，真实 IMAP 同步请用容器化部署'
    : '当前部署形态未提供邮件同步服务，请改用容器化或长驻进程部署'
  await persist()
  return json({ ok: true, id, lastSyncAt: a.lastSyncAt, note: a.lastSyncError }, 202)
}

async function handleAIChat(request, env) {
  if (request.method !== 'POST') return err('method not allowed', 405)
  const body = await readJSON(request)
  let last = ''
  for (const m of body.messages || []) {
    if (m.role === 'user') last = m.content || ''
  }
  let payload = last
  let redacted = false
  if (body.tenantTier === 'public' || body.sensitivity === 1) {
    const r = redact(payload)
    payload = r.text
    redacted = r.changed
  }

  let backend = 'local-demo'
  let content = ''
  // 上游优先级：显式配置的第三方端点 > 平台注入的 AI Gateway 变量。
  // 两者均为 OpenAI 兼容的 /chat/completions 接口。
  const upstream = (env && (env.AI_THIRD_PARTY_URL || env.AI_GATEWAY_BASE_URL)) || ''
  const key = (env && (env.AI_THIRD_PARTY_KEY || env.AI_GATEWAY_API_KEY)) || ''
  if (upstream && key) {
    try {
      const resp = await fetch(`${upstream.replace(/\/+$/, '')}/chat/completions`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` },
        body: JSON.stringify({
          model: (env && env.AI_THIRD_PARTY_MODEL) || 'gpt-4o-mini',
          messages: [{ role: 'user', content: payload }],
          max_tokens: 512,
        }),
      })
      if (!resp.ok) throw new Error(`upstream ${resp.status}`)
      const data = await resp.json()
      content = data?.choices?.[0]?.message?.content || ''
      backend = 'third-party'
    } catch (e) {
      if (!demoEnabled) {
        return json({ error: `上游模型调用失败：${e && e.message ? e.message : String(e)}` }, 502)
      }
      content = `[demo] 上游模型调用失败，已降级为本地演示回复。你问的是：${payload}`
    }
  }
  if (!content) {
    if (!demoEnabled) {
      // 生产形态：宁可报错，也不伪造一条看似正常的 AI 回答。
      return json({ error: '未配置 AI 上游（AI_GATEWAY_BASE_URL / AI_GATEWAY_API_KEY）' }, 503)
    }
    content = `[demo] 这是本地演示后端（未配置真实模型端点）。你问的是：${payload}。可在演示邮箱中检索 Welcome / Invoice / meeting 验证全文检索。`
  }
  return json({
    content,
    backend,
    model: 'demo-echo',
    tokensIn: payload.length,
    tokensOut: content.length,
    redacted,
  })
}

async function handleDemoPush(request, url) {
  if (!demoEnabled) return err('not found', 404)
  if (request.method !== 'POST') return err('method not allowed', 405)
  const accountId = url.searchParams.get('accountId') || DEFAULT_ACCOUNT
  if (!state.accounts.has(accountId)) return err('account not found', 404)
  const now = Math.floor(Date.now() / 1000)
  const id = `push-${Date.now()}-${Math.floor(Math.random() * 1e6)}`
  const subject = `演示推送邮件 ${new Date().toISOString().slice(11, 19)}`
  const m = {
    id,
    accountId,
    provider: 'imap',
    folder: 'INBOX',
    from: { name: '系统通知', email: 'noreply@system.com' },
    to: [{ name: '', email: 'me@example.com' }],
    cc: [],
    bcc: [],
    subject,
    bodyText: '这是一封由 /api/demo/push 生成的演示邮件。',
    snippet: '这是一封由 /api/demo/push 生成的演示邮件。',
    hasAttachment: false,
    attachments: [],
    internalDate: now,
    sizeBytes: 96,
    cursor: { lastUid: now },
    read: false,
  }
  state.mails.set(id, m)
  await persist()
  return json({ ok: true, id, subject })
}

// ------------------------------------------------------------------ 入口

export async function onRequest({ request, env }) {
  // 每次请求按环境变量刷新开关（Serverless 实例可能复用，不能只在冷启动读一次）
  demoEnabled = !(env && env.ENABLE_DEMO === 'false')
  await ensure()
  const url = new URL(request.url)
  // 归一化：去掉 /api 前缀，得到 ["mails","m1","read"] 形式的片段
  const parts = url.pathname.replace(/^\/api\/?/, '').split('/').filter(Boolean)

  try {
    if (parts.length === 1 && parts[0] === 'health') {
      return json({
        ok: true,
        ts: Date.now(),
        mode: `serverless-${mode}`,
        demo: demoEnabled,
        // 与 Go 主干（src/version）保持同一版本号：多端部署按版本可追踪。
        version: APP_VERSION,
        versionCode: APP_VERSION_CODE,
        accounts: state.accounts.size,
        mails: state.mails.size,
      })
    }
    if (parts.length === 1 && parts[0] === 'mails') return handleMails(request, url)
    if (parts.length === 2 && parts[0] === 'mails') return handleMailByID(request, parts[1])
    if (parts.length === 3 && parts[0] === 'mails' && parts[2] === 'read')
      return handleSetRead(request, url, parts[1])
    if (parts.length === 1 && parts[0] === 'search') return handleSearch(request, url)
    if (parts.length === 1 && parts[0] === 'accounts') return handleAccounts(request)
    if (parts.length === 2 && parts[0] === 'accounts') return handleAccountByID(request, parts[1])
    if (parts.length === 3 && parts[0] === 'accounts' && parts[2] === 'credentials') {
      if (request.method !== 'POST') return err('method not allowed', 405)
      return json({
        ok: true,
        note: demoEnabled
          ? '演示形态：凭据仅登记，不发起真实 IMAP 连接'
          : '凭据已登记（当前部署形态未发起真实 IMAP 连接）',
      })
    }
    if (parts.length === 3 && parts[0] === 'accounts' && parts[2] === 'sync')
      return handleAccountSync(request, parts[1])
    if (parts.length === 2 && parts[0] === 'ai' && parts[1] === 'chat')
      return handleAIChat(request, env)
    if (parts.length === 2 && parts[0] === 'demo' && parts[1] === 'push')
      return handleDemoPush(request, url)
  } catch (e) {
    return err(`internal error: ${e && e.message ? e.message : String(e)}`, 500)
  }

  return err('not found', 404)
}
