// 通过 CDP 驱动已打开的浏览器，逐断点验证响应式布局是否按设计规范生效。
// 之所以走 CDP 而非 agent-browser viewport：后者每次 CLI 调用都是新会话，视口设置不保留。
const CDP_BROWSER = process.argv[2]
const URL_TOKEN = process.argv[3]

async function main() {
  // 找到目标页面 target
  const httpBase = CDP_BROWSER.replace('ws://', 'http://').replace(/\/devtools\/.*/, '')
  const targets = await fetch(`${httpBase}/json/list`).then((r) => r.json())
  const page = targets.find((t) => t.type === 'page' && t.url.includes(URL_TOKEN))
  if (!page) throw new Error('未找到目标页面 target')

  const ws = new WebSocket(page.webSocketDebuggerUrl)
  let id = 0
  const pending = new Map()

  const send = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const msgId = ++id
      pending.set(msgId, { resolve, reject })
      ws.send(JSON.stringify({ id: msgId, method, params }))
    })

  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data)
    if (m.id && pending.has(m.id)) {
      const { resolve, reject } = pending.get(m.id)
      pending.delete(m.id)
      m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result)
    }
  }

  await new Promise((res) => (ws.onopen = res))

  const evalJs = async (expr) => {
    const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true })
    return r.result.value
  }

  const cases = [
    { w: 1440, h: 900, label: '桌面宽屏' },
    { w: 1200, h: 900, label: '桌面常规' },
    { w: 1000, h: 900, label: '平板横屏' },
    { w: 880, h: 900, label: '平板竖屏' },
    { w: 620, h: 900, label: '手机' },
  ]

  console.log('断点'.padEnd(10), '视口'.padEnd(10), 'content列'.padEnd(26), 'sidebar'.padEnd(22), 'nav方向')
  console.log('-'.repeat(96))

  for (const c of cases) {
    await send('Emulation.setDeviceMetricsOverride', {
      width: c.w,
      height: c.h,
      deviceScaleFactor: 1,
      mobile: c.w < 640,
    })
    await new Promise((r) => setTimeout(r, 350))
    const out = await evalJs(`JSON.stringify({
      w: innerWidth,
      cols: getComputedStyle(document.querySelector('.content')).gridTemplateColumns,
      sbPos: getComputedStyle(document.querySelector('.sidebar')).position,
      sbW: getComputedStyle(document.querySelector('.sidebar')).width,
      navDir: getComputedStyle(document.querySelector('.account-nav')).flexDirection,
      appPad: getComputedStyle(document.querySelector('.app')).paddingLeft,
      modalHeadP: getComputedStyle(document.querySelector('.app-header')).paddingLeft
    })`)
    const d = JSON.parse(out)
    console.log(
      c.label.padEnd(10),
      `${c.w}px`.padEnd(10),
      d.cols.padEnd(26),
      `${d.sbPos} ${d.sbW}`.padEnd(22),
      d.navDir,
      `| app ${d.appPad}`
    )
  }

  // 清理覆盖，恢复原始尺寸
  await send('Emulation.clearDeviceMetricsOverride')
  ws.close()
}

main().catch((e) => {
  console.error('FAILED:', e.message)
  process.exit(1)
})
