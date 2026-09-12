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
// 与 CSS .mail-item { height: 80px } 保持同步（含 padding + border-bottom）。
const ROW_HEIGHT = 80
// 可视区上下各多渲染的缓冲行数，减少快速滚动时的白屏闪烁。
const BUFFER = 5
// 超过此阈值才启用虚拟滚动；小列表全量渲染避免过度工程。
const VIRTUAL_THRESHOLD = 50

function fmtDate(ts: number): string {
  if (!ts) return ''
  // internalDate 为 Unix 秒（Go 版 demo 用 time.Now().Unix()）
  const d = new Date(ts * 1000)
  return d.toLocaleString()
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
    if (hits.length === 0) return <p className="empty">无检索命中</p>
    return (
      <ul className="mail-list">
        {hits.map((h) => (
          <li
            key={h.idempotencyKey}
            className="mail-item clickable"
            role="button"
            tabIndex={0}
            onClick={() => onSelectHit?.(h)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') onSelectHit?.(h)
            }}
          >
            <div className="mail-subject">{h.subject || '(无主题)'}</div>
            <div className="mail-meta">
              来自 {h.from} · {fmtDate(h.internalDate)}
            </div>
            <div className="mail-preview">{h.preview}</div>
          </li>
        ))}
      </ul>
    )
  }

  if (sorted.length === 0) return <p className="empty">{loading ? '加载中…' : '暂无邮件'}</p>

  // ── 虚拟滚动计算 ──
  const total = sorted.length
  const virtual = total > VIRTUAL_THRESHOLD
  const startIndex = virtual
    ? Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - BUFFER)
    : 0
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
      onScroll={
        virtual
          ? (e) => setScrollTop(e.currentTarget.scrollTop)
          : undefined
      }
      style={
        virtual
          ? { paddingTop: topPad, paddingBottom: bottomPad }
          : undefined
      }
    >
      {visible.map((m, i) => (
        <li
          key={m.id}
          className={`mail-item clickable ${m.read ? '' : 'unread'}`}
          role="button"
          tabIndex={0}
          aria-posinset={virtual ? startIndex + i + 1 : undefined}
          aria-setsize={virtual ? total : undefined}
          onClick={() => onSelectMail?.(m)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') onSelectMail?.(m)
          }}
        >
          {!m.read && <span className="unread-dot" aria-label="未读" />}
          <div className="mail-subject">{m.subject || '(无主题)'}</div>
          <div className="mail-meta">
            来自 {m.from.email} · {fmtDate(m.internalDate)}
            {m.hasAttachment ? (
              <>
                {' · '}
                <Icon name="paperclip" size={12} className="attach-icon-inline" />
              </>
            ) : ''}
          </div>
          <div className="mail-preview">{m.snippet || m.bodyText?.slice(0, 120) || ''}</div>
        </li>
      ))}
    </ul>
  )
}
