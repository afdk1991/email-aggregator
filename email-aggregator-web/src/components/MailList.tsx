import { useEffect, useMemo, useRef, useState } from 'react'
import type { CanonicalMail, SearchHit } from '../types'
import { Icon } from './Icon'

interface Props {
  mails?: CanonicalMail[]
  hits?: SearchHit[]
  loading?: boolean
  onSelectMail?: (m: CanonicalMail) => void
  onSelectHit?: (h: SearchHit) => void
}

// ── 虚拟滚动常量 ──────────────────────────────────────────
// 与 CSS .mail-item { height: 76px } 保持同步（含 padding + border-bottom）。
const ROW_HEIGHT = 76
// 可视区上下各多渲染的缓冲行数，减少快速滚动时的白屏闪烁。
const BUFFER = 5
// 超过此阈值才启用虚拟滚动；小列表全量渲染避免过度工程。
const VIRTUAL_THRESHOLD = 50

const DAY = 86400

// 时间表达分级：今天只给时刻、近七天给星期、更早给日期。
// 比统一 toLocaleString() 更省横向空间，也让"新邮件"在视觉上自然凸显。
function fmtTime(ts: number): string {
  if (!ts) return ''
  const d = new Date(ts * 1000)
  const now = new Date()
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime() / 1000
  const diffDays = Math.floor((startOfToday - ts) / DAY)

  if (ts >= startOfToday) {
    return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
  }
  if (diffDays < 6) {
    return d.toLocaleDateString('zh-CN', { weekday: 'short' })
  }
  if (d.getFullYear() === now.getFullYear()) {
    return d.toLocaleDateString('zh-CN', { month: 'numeric', day: 'numeric' })
  }
  return d.toLocaleDateString('zh-CN', { year: '2-digit', month: 'numeric', day: 'numeric' })
}

// 三行式邮件行：主题行 → 发件人 · 时间 → 摘要。
// 发件人与时间合并为一行，去掉重复的"来自"前缀，信息密度更高、扫读更快。
function MailRow({
  subject,
  from,
  ts,
  preview,
  unread,
  hasAttachment,
  ariaPosinset,
  ariaSetsize,
  onSelect,
}: {
  subject: string
  from: string
  ts: number
  preview: string
  unread: boolean
  hasAttachment?: boolean
  ariaPosinset?: number
  ariaSetsize?: number
  onSelect: () => void
}) {
  return (
    <li
      className={`mail-item clickable${unread ? ' unread' : ''}`}
      role="button"
      tabIndex={0}
      aria-posinset={ariaPosinset}
      aria-setsize={ariaSetsize}
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onSelect()
        }
      }}
    >
      <div className="mail-item-head">
        {/* 未读圆点：与左侧竖条、加粗标题构成三重冗余，不只靠颜色传达状态 */}
        {unread && <span className="unread-dot" aria-label="未读" />}
        <span className="mail-subject">{subject}</span>
      </div>
      <div className="mail-meta">
        <span className="mail-from">{from}</span>
        {hasAttachment && <Icon name="paperclip" size={12} className="attach-icon-inline" />}
        <span className="mail-time">{fmtTime(ts)}</span>
      </div>
      <div className="mail-preview">{preview}</div>
    </li>
  )
}

export default function MailList({ mails, hits, loading, onSelectMail, onSelectHit }: Props) {
  const listRef = useRef<HTMLUListElement>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [viewportH, setViewportH] = useState(600)

  // 收件箱排序：未读上移、已读下移（稳定排序 → 组内保持服务端原顺序）。
  // 任何已读状态变更（打开即读 / 详情切换 / WS mail-updated）都会更新 mails 触发本 memo 重算，
  // 已读邮件自动下移、未读邮件自动上移。检索命中不排序（保持相关性顺序）。
  const sorted = useMemo(() => {
    const list = mails ?? []
    if (list.length === 0) return list
    return [...list].sort((a, b) => (a.read ? 1 : 0) - (b.read ? 1 : 0))
  }, [mails])

  // 仅虚拟列表需要测量视口高度（ResizeObserver），hits 分支不挂载此 ref。
  useEffect(() => {
    if (!listRef.current) return
    const el = listRef.current
    const measure = () => setViewportH(el.clientHeight)
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [sorted.length])

  // ── 检索命中分支：不虚拟化（结果集通常较小） ──
  if (hits) {
    if (hits.length === 0) {
      return (
        <div className="empty-state">
          <Icon name="search" size={22} />
          <p>没有匹配的邮件</p>
          <span>试试更换关键词，或按主题、发件人检索</span>
        </div>
      )
    }
    return (
      <ul className="mail-list">
        {hits.map((h) => (
          <MailRow
            key={h.idempotencyKey}
            subject={h.subject || '(无主题)'}
            from={h.from}
            ts={h.internalDate}
            preview={h.preview}
            unread={false}
            onSelect={() => onSelectHit?.(h)}
          />
        ))}
      </ul>
    )
  }

  // ── 空状态 / 加载状态 ──
  if (sorted.length === 0) {
    if (loading) {
      return (
        <div className="empty-state">
          <Icon name="sync" size={22} />
          <p>正在加载邮件…</p>
        </div>
      )
    }
    return (
      <div className="empty-state">
        <Icon name="envelope" size={22} />
        <p>收件箱是空的</p>
        <span>添加账户并设置凭据后，点击「立即同步」拉取邮件</span>
      </div>
    )
  }

  // ── 虚拟滚动计算 ──
  const total = sorted.length
  const virtual = total > VIRTUAL_THRESHOLD
  const startIndex = virtual ? Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - BUFFER) : 0
  const endIndex = virtual
    ? Math.min(total, Math.ceil((scrollTop + viewportH) / ROW_HEIGHT) + BUFFER)
    : total
  const visible = virtual ? sorted.slice(startIndex, endIndex) : sorted
  const topPad = virtual ? startIndex * ROW_HEIGHT : 0
  const bottomPad = virtual ? (total - endIndex) * ROW_HEIGHT : 0

  return (
    <ul
      ref={virtual ? listRef : undefined}
      className={`mail-list${virtual ? ' virtual' : ''}`}
      style={virtual ? { paddingTop: topPad, paddingBottom: bottomPad } : undefined}
      onScroll={virtual ? (e) => setScrollTop(e.currentTarget.scrollTop) : undefined}
    >
      {visible.map((m, i) => (
        <MailRow
          key={m.id}
          subject={m.subject || '(无主题)'}
          from={m.from.name || m.from.email}
          ts={m.internalDate}
          preview={m.snippet || m.bodyText?.slice(0, 120) || ''}
          unread={!m.read}
          hasAttachment={m.hasAttachment}
          ariaPosinset={virtual ? startIndex + i + 1 : undefined}
          ariaSetsize={virtual ? total : undefined}
          onSelect={() => onSelectMail?.(m)}
        />
      ))}
    </ul>
  )
}
