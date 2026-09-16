// 可访问性抽检：焦点可见性、触摸目标尺寸、对比度、语义标签。
// 同样是走 CDP —— 需要真实布局盒模型（getBoundingClientRect）与计算色值。
const CDP_BROWSER = process.argv[2]
const URL_TOKEN = process.argv[3]

// WCAG 相对亮度
function lum([r, g, b]) {
  const f = (c) => {
    c /= 255
    return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4)
  }
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b)
}
function ratio(a, b) {
  const [l1, l2] = [lum(a), lum(b)].sort((x, y) => y - x)
  return (l1 + 0.05) / (l2 + 0.05)
}
const parseRGB = (s) => {
  const m = s.match(/rgba?\(([^)]+)\)/)
  if (!m) return null
  const p = m[1].split(',').map((v) => parseFloat(v))
  return [p[0], p[1], p[2], p[3] === undefined ? 1 : p[3]]
}

async function main() {
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

  // 1) 触摸目标尺寸（WCAG 2.5.8 AA ≥ 24×24，设计自定 ≥ 32/36）
  const targetsInfo = JSON.parse(
    await evalJs(`JSON.stringify(
      ['.btn','.icon-btn','.account-chip','.search-clear','.modal-close','.chip-badge','.badge','.count-badge']
        .flatMap(sel => Array.from(document.querySelectorAll(sel)).map(el => {
          const r = el.getBoundingClientRect()
          return { sel, w: Math.round(r.width), h: Math.round(r.height) }
        }))
    )`)
  )
  console.log('=== 1. 触摸/点击目标尺寸 ===')
  if (targetsInfo.length === 0) console.log('（页面上暂无可交互元素）')
  const seen = new Set()
  for (const t of targetsInfo) {
    if (seen.has(t.sel)) continue
    seen.add(t.sel)
    const ok = t.h >= 24
    console.log(`${ok ? 'PASS' : 'FAIL'}  ${t.sel.padEnd(16)} ${t.w}×${t.h}px`)
  }

  // 2) 焦点可见性：逐元素 focus 后比对 outline/box-shadow
  const focusCheck = JSON.parse(
    await evalJs(`(() => {
      const res = []
      for (const sel of ['.btn--primary','.account-chip','input','textarea','.modal-close','.search-clear']) {
        const el = document.querySelector(sel)
        if (!el) { res.push({sel, found:false}); continue }
        // 用 :focus-visible 不易脚本触发，改比对 CSS 规则是否声明了焦点样式
        let outlineRule = false, shadowRule = false
        for (const sheet of document.styleSheets) {
          let rules; try { rules = sheet.cssRules } catch { continue }
          for (const r of rules) {
            if (!r.selectorText) continue
            if (!/:focus/.test(r.selectorText)) continue
            if (r.selectorText.includes(sel) || r.selectorText.includes(':focus-visible')) {
              if (r.style.outline || r.style.outlineWidth) outlineRule = true
              if (r.style.boxShadow) shadowRule = true
            }
          }
        }
        res.push({sel, found:true, outlineRule, shadowRule})
      }
      return JSON.stringify(res)
    })()`)
  )
  console.log('\n=== 2. 焦点可见性（是否声明 outline / box-shadow）===')
  for (const f of focusCheck) {
    if (!f.found) { console.log(`SKIP  ${f.sel} （未渲染）`); continue }
    const ok = f.outlineRule || f.shadowRule
    console.log(`${ok ? 'PASS' : 'FAIL'}  ${f.sel.padEnd(16)} outline=${f.outlineRule} shadow=${f.shadowRule}`)
  }

  // 3) 对比度：正文/辅助文本 vs 其背景
  const contrast = JSON.parse(
    await evalJs(`(() => {
      const out = []
      const probe = (label, el) => {
        if (!el) { out.push({label, missing:true}); return }
        const cs = getComputedStyle(el)
        // 向上找到第一个非透明背景
        let bg = cs.backgroundColor, node = el
        while (bg === 'rgba(0, 0, 0, 0)' && node.parentElement) {
          node = node.parentElement
          bg = getComputedStyle(node).backgroundColor
        }
        out.push({label, color: cs.color, bg, size: cs.fontSize})
      }
      probe('正文 body', document.body)
      probe('辅助 .panel-sub', document.querySelector('.panel-sub'))
      probe('组标题 .group-title', document.querySelector('.group-title'))
      probe('主按钮 .btn--primary', document.querySelector('.btn--primary'))
      probe('徽标 .badge', document.querySelector('.badge'))
      probe('空状态说明', document.querySelector('.empty-state span'))
      return JSON.stringify(out)
    })()`)
  )
  console.log('\n=== 3. 文本对比度（WCAG AA 正文 ≥4.5:1 / 大字 ≥3:1）===')
  for (const c of contrast) {
    if (c.missing) { console.log(`SKIP  ${c.label}`); continue }
    const fg = parseRGB(c.color)
    const bg = parseRGB(c.bg)
    if (!fg || !bg) { console.log(`SKIP  ${c.label} （色值不可解析）`); continue }
    const r = ratio(fg, bg)
    const px = parseFloat(c.size)
    const large = px >= 18.66
    const need = large ? 3 : 4.5
    console.log(
      `${r >= need ? 'PASS' : 'FAIL'}  ${c.label.padEnd(20)} ${r.toFixed(2)}:1 (需 ≥${need}) ${c.size}`
    )
  }

  // 4) 语义标签：可交互元素是否有可读名称
  const a11y = JSON.parse(
    await evalJs(`(() => {
      const els = Array.from(document.querySelectorAll('button, input, textarea, select, a[href], [role="button"]'))
      const unnamed = []
      for (const el of els) {
        const name = (el.getAttribute('aria-label') || el.textContent || el.getAttribute('title') || '').trim()
        if (!name) unnamed.push(el.tagName + (el.id ? '#' + el.id : '') + '.' + (el.className || '').toString().split(' ')[0])
      }
      return JSON.stringify({ total: els.length, unnamed })
    })()`)
  )
  console.log('\n=== 4. 可交互元素可读名称 ===')
  console.log(`总数 ${a11y.total}，无名称 ${a11y.unnamed.length}`)
  for (const n of a11y.unnamed) console.log(`FAIL  缺少可读名称: ${n}`)
  if (a11y.unnamed.length === 0) console.log('PASS  所有可交互元素均有可读名称')

  ws.close()
}
main().catch((e) => {
  console.error('FAILED:', e.message)
  process.exit(1)
})
