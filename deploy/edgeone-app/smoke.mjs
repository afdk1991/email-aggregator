// 本地冒烟测试：直接驱动云函数 onRequest，验证全部路由与契约。
// 用法：node deploy/edgeone-app/smoke.mjs
import { onRequest } from './cloud-functions/api/[[default]].js'

const BASE = 'https://example.invalid'
// --prod：以生产形态（ENABLE_DEMO=false）跑另一套断言，验证"线上无演示数据"。
// 必须单独起进程：云函数状态是模块级的，同进程内切开关不会重新播种。
const PROD = process.argv.includes('--prod')
let pass = 0
let fail = 0

function check(name, cond, extra = '') {
  if (cond) {
    pass++
    console.log(`PASS  ${name}`)
  } else {
    fail++
    console.log(`FAIL  ${name} ${extra}`)
  }
}

function req(method, path, body, env) {
  const url = new URL(BASE + path)
  const init = { method, headers: {} }
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  return onRequest({ request: new Request(url, init), env: env || {} })
}

async function getJSON(method, path, body, env) {
  const res = await req(method, path, body, env)
  const text = await res.text()
  let data = null
  try {
    data = JSON.parse(text)
  } catch {
    data = { __raw: text.slice(0, 120) }
  }
  return { status: res.status, data }
}

// ---------------------------------------------------------------- 生产形态套件
if (PROD) {
  const ENV = { ENABLE_DEMO: 'false' }
  const call = (m, p, b) => getJSON(m, p, b, ENV)

  const h = await call('GET', '/api/health')
  check('[prod] health 标记 demo=false', h.status === 200 && h.data.demo === false, JSON.stringify(h.data))

  const acc = await call('GET', '/api/accounts')
  check('[prod] 无预置种子账户', acc.status === 200 && acc.data.accounts?.length === 0, JSON.stringify(acc.data))

  const mails = await call('GET', '/api/mails?accountId=acc_demo')
  check('[prod] 无预置种子邮件', mails.data.count === 0, `count=${mails.data.count}`)

  const push = await call('POST', '/api/demo/push?accountId=acc_demo')
  check('[prod] /api/demo/push → 404', push.status === 404, `status=${push.status}`)

  const ai = await call('POST', '/api/ai/chat', {
    tenantId: 'default', tenantTier: 'public', capability: 'chat',
    sensitivity: 1, userConsented: true,
    messages: [{ role: 'user', content: '你好' }],
  })
  check('[prod] AI 无上游 → 503 且不含 [demo] 伪造回复', ai.status === 503 && !String(ai.data.content || '').includes('[demo]'), JSON.stringify(ai.data))

  const created = await call('POST', '/api/accounts', { id: 'acc_real', provider: 'imap', email: 'me@corp.com' })
  check('[prod] 仍可创建真实账户', created.status === 201, JSON.stringify(created.data))

  const del = await call('DELETE', '/api/accounts/acc_real')
  check('[prod] 删除账户生效', del.status === 200 && del.data.ok === true)

  console.log(`\n结果（生产形态）：${pass} 通过 / ${fail} 失败`)
  process.exit(fail === 0 ? 0 : 1)
}

// 1. health
{
  const { status, data } = await getJSON('GET', '/api/health')
  check('GET /api/health → 200 且 ok=true', status === 200 && data.ok === true, JSON.stringify(data))
  check('health 暴露 storage mode', typeof data.mode === 'string' && data.mode.startsWith('serverless-'), data.mode)
}

// 2. accounts
{
  const { status, data } = await getJSON('GET', '/api/accounts')
  const ids = (data.accounts || []).map((a) => a.id)
  check('GET /api/accounts → 3 个账户', status === 200 && data.accounts?.length === 3, JSON.stringify(ids))
  check('账户含 unread 计数', data.accounts?.[0]?.unread !== undefined)
  // 已下线的演示账户不得再出现
  const retired = ['acc_demo3', 'acc_demo32', 'acc_personal1', 'acc_personal11', 'acc_work1', 'acc_work11']
  check('6 个演示账户已清除', retired.every((id) => !ids.includes(id)), JSON.stringify(ids))
  // 显式重建也要被拒绝，避免被重新生成
  const { status: rs } = await getJSON('POST', '/api/accounts', { id: 'acc_demo3', provider: 'imap' })
  check('重建已下线账户被拒绝', rs === 400, `status=${rs}`)
}

// 3. mails
{
  const { status, data } = await getJSON('GET', '/api/mails?accountId=acc_demo&folder=INBOX&limit=50')
  check('GET /api/mails(acc_demo) → 3 封', status === 200 && data.count === 3, JSON.stringify(data).slice(0, 200))
  check('邮件含 from/subject/bodyText', !!data.mails?.[0]?.from?.email && !!data.mails?.[0]?.bodyText)
}

// 4. mails 按账户隔离
{
  const { data } = await getJSON('GET', '/api/mails?accountId=acc_work')
  check('acc_work 独立返回 2 封', data.count === 2, JSON.stringify(data).slice(0, 160))
}

// 5. 单封邮件
{
  const { status, data } = await getJSON('GET', '/api/mails/m1?accountId=acc_demo')
  check('GET /api/mails/m1 → 命中', status === 200 && data.id === 'm1')
}

// 6. 标记已读
{
  const { status, data } = await getJSON('POST', '/api/mails/m1/read?accountId=acc_demo', { read: true })
  check('POST /api/mails/m1/read → read=true', status === 200 && data.read === true)
  const after = await getJSON('GET', '/api/mails/m1')
  check('已读状态已持久化到内存态', after.data.read === true)
}

// 7. 检索
{
  const { status, data } = await getJSON('GET', '/api/search?accountId=acc_demo&q=Invoice')
  check('GET /api/search?q=Invoice → 命中 1', status === 200 && data.hits.length === 1, JSON.stringify(data.hits))
  const zh = await getJSON('GET', '/api/search?accountId=acc_work&q=复盘')
  check('中文检索命中', zh.data.hits.length === 1, JSON.stringify(zh.data.hits))
}

// 8. AI chat（demo 后端）
{
  const { status, data } = await getJSON('POST', '/api/ai/chat', {
    tenantId: 'default', tenantTier: 'public', capability: 'chat',
    sensitivity: 1, userConsented: true,
    messages: [{ role: 'user', content: '联系我 alice@example.com 或 13800138000' }],
  })
  check('POST /api/ai/chat → 200 有 content', status === 200 && typeof data.content === 'string' && data.content.length > 0)
  check('AI 出网前已脱敏', data.redacted === true && !data.content.includes('alice@example.com'), JSON.stringify(data))
}

// 9. demo push
{
  const { status, data } = await getJSON('POST', '/api/demo/push?accountId=acc_demo')
  check('POST /api/demo/push → 生成新邮件', status === 200 && data.ok === true && !!data.id)
  const after = await getJSON('GET', '/api/mails?accountId=acc_demo')
  check('列表已增至 4 封', after.data.count === 4, `count=${after.data.count}`)
}

// 10. 账户创建 / 更新 / 删除
{
  const created = await getJSON('POST', '/api/accounts', { id: 'acc_new', provider: 'imap', email: 'n@x.com' })
  check('POST /api/accounts → 201', created.status === 201 && created.data.id === 'acc_new', JSON.stringify(created.data))

  const upd = await getJSON('PUT', '/api/accounts/acc_new', { status: 'paused' })
  check('PUT 部分更新保留 provider', upd.status === 200 && upd.data.status === 'paused' && upd.data.provider === 'imap', JSON.stringify(upd.data))

  const del = await getJSON('DELETE', '/api/accounts/acc_new')
  check('DELETE /api/accounts/acc_new → ok', del.status === 200 && del.data.ok === true)
  const gone = await getJSON('GET', '/api/accounts/acc_new')
  check('删除后不可见', gone.status === 404)
}

// 11. 未知路径
{
  const { status, data } = await getJSON('GET', '/api/nope')
  check('未知 /api 路径 → 404', status === 404 && !!data.error)
}

console.log(`\n结果：${pass} 通过 / ${fail} 失败`)
process.exit(fail === 0 ? 0 : 1)
