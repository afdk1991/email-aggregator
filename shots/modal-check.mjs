// 验证邮件详情模态的新结构（固定头/滚动体/固定底栏 + dl 网格元信息）
const CDP = process.argv[2]
const TOKEN = process.argv[3]

async function main() {
  const base = CDP.replace('ws://', 'http://').replace(/\/devtools\/.*/, '')
  const targets = await fetch(`${base}/json/list`).then((r) => r.json())
  const page = targets.find((t) => t.type === 'page' && t.url.includes(TOKEN))
  const ws = new WebSocket(page.webSocketDebuggerUrl)
  let id = 0
  const pend = new Map()
  const send = (m, pr = {}) =>
    new Promise((res, rej) => {
      const i = ++id
      pend.set(i, { res, rej })
      ws.send(JSON.stringify({ id: i, method: m, params: pr }))
    })
  ws.onmessage = (e) => {
    const m = JSON.parse(e.data)
    if (m.id && pend.has(m.id)) {
      const { res, rej } = pend.get(m.id)
      pend.delete(m.id)
      m.error ? rej(new Error(JSON.stringify(m.error))) : res(m.result)
    }
  }
  await new Promise((r) => (ws.onopen = r))
  const ev = async (x) => (await send('Runtime.evaluate', { expression: x, returnByValue: true })).result.value

  const sels = [
    '.modal-overlay',
    '.modal',
    '.modal-head',
    '.modal-body',
    '.modal-foot',
    '.modal-close',
    '.mail-detail-subject',
    '.mail-detail-meta',
    '.mail-detail-body',
    '.read-state',
    '.attachments',
  ]
  console.log('=== 详情模态结构（元素计数）===')
  for (const s of sels) console.log(s.padEnd(24), await ev(`document.querySelectorAll('${s}').length`))

  console.log()
  console.log('meta 容器标签     :', await ev(`document.querySelector('.mail-detail-meta')?.tagName`))
  console.log('dt / dd 数量      :', await ev(`document.querySelectorAll('.mail-detail-meta dt').length`), '/', await ev(`document.querySelectorAll('.mail-detail-meta dd').length`))
  console.log('meta 布局         :', await ev(`getComputedStyle(document.querySelector('.mail-detail-meta')).display`))
  console.log('meta 列定义       :', await ev(`getComputedStyle(document.querySelector('.mail-detail-meta')).gridTemplateColumns`))
  console.log('主题              :', await ev(`document.querySelector('.mail-detail-subject')?.textContent`))
  console.log('底部按钮          :', await ev(`JSON.stringify(Array.from(document.querySelectorAll('.modal-foot .btn')).map(b => b.textContent.trim()))`))
  console.log('模态圆角          :', await ev(`getComputedStyle(document.querySelector('.modal')).borderRadius`))
  console.log('模态最大宽        :', await ev(`getComputedStyle(document.querySelector('.modal')).maxWidth`))
  console.log('遮罩 backdrop     :', await ev(`getComputedStyle(document.querySelector('.modal-overlay')).backdropFilter`))
  console.log('body 可滚动       :', await ev(`getComputedStyle(document.querySelector('.modal-body')).overflowY`))
  console.log('foot 背景         :', await ev(`getComputedStyle(document.querySelector('.modal-foot')).backgroundColor`))
  console.log('关闭按钮焦点      :', await ev(`document.activeElement?.className`))

  ws.close()
}
main().catch((e) => {
  console.error('FAILED:', e.message)
  process.exit(1)
})
