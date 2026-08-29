import { useMemo } from 'react'
import type { CanonicalMail, SearchHit } from '../types'

interface Props {
  mails?: CanonicalMail[]
  hits?: SearchHit[]
  loading?: boolean
  onSelectMail?: (m: CanonicalMail) => void
  onSelectHit?: (h: SearchHit) => void
}

function fmtDate(ts: number): string {
  if (!ts) return ''
  // internalDate 为 Unix 秒（Go 版 demo 用 time.Now().Unix()）
  const d = new Date(ts * 1000)
  return d.toLocaleString()
}

export default function MailList({ mails, hits, loading, onSelectMail, onSelectHit }: Props) {
  // 收件箱排序：未读上移、已读下移（稳定排序 → 组内保持服务端原顺序）。
  // 任何已读状态变更（打开即读 / 详情切换 / WS mail-updated）都会更新 mails 触发本 memo 重算，
  // 已读邮件自动下移、未读邮件自动上移。检索命中不排序（保持相关性顺序）。
  const sorted = useMemo(() => {
    const list = mails ?? []
    if (list.length === 0) return list
    return [...list].sort((a, b) => (a.read ? 1 : 0) - (b.read ? 1 : 0))
  }, [mails])

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
  return (
      <ul className="mail-list">
        {sorted.map((m) => (
          <li
            key={m.id}
            className={`mail-item clickable ${m.read ? '' : 'unread'}`}
            role="button"
            tabIndex={0}
            onClick={() => onSelectMail?.(m)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') onSelectMail?.(m)
            }}
          >
            {!m.read && <span className="unread-dot" aria-label="未读" />}
            <div className="mail-subject">{m.subject || '(无主题)'}</div>
            <div className="mail-meta">
              来自 {m.from.email} · {fmtDate(m.internalDate)}
              {m.hasAttachment ? ' · 📎' : ''}
            </div>
            <div className="mail-preview">{m.snippet || m.bodyText?.slice(0, 120) || ''}</div>
          </li>
        ))}
      </ul>
  )
}
