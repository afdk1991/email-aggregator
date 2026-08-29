import { useEffect } from 'react'
import type { CanonicalMail, SearchHit } from '../types'

interface Props {
  mail?: CanonicalMail | null
  hit?: SearchHit | null
  onClose: () => void
  onToggleRead?: (mail: CanonicalMail) => void
  onDelete?: (mail: CanonicalMail) => void
}

function fmtDate(ts: number): string {
  if (!ts) return ''
  return new Date(ts * 1000).toLocaleString()
}

// 邮件详情模态：点击列表项打开，展示完整邮件内容。
// mail 优先（含正文/收件人/附件），命中项 hit 仅含预览。
// 仅当存在权威 mail 时提供「标记已读/未读」「删除」操作。
export default function MailDetail({ mail, hit, onClose, onToggleRead, onDelete }: Props) {
  // Esc 关闭模态（仅在打开时注册监听，避免全局常驻）
  useEffect(() => {
    if (!mail && !hit) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [mail, hit, onClose])

  if (!mail && !hit) return null

  const subject = mail?.subject || hit?.subject || '(无主题)'
  const from = mail ? mail.from.name || mail.from.email : hit?.from || ''
  const date = mail ? mail.internalDate : hit?.internalDate ?? 0
  const to = mail?.to?.map((t) => t.email).join(', ') || '—'
  const body = mail?.bodyText || mail?.bodyHTML || hit?.preview || ''
  const previewFallback = hit && !mail ? '(检索命中预览，点开完整内容需权威资源)' : '(无正文)'
  const displayBody = body || previewFallback

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <button className="modal-close" onClick={onClose} aria-label="关闭">
          ×
        </button>
        <h3 className="mail-detail-subject">{subject}</h3>
        <div className="mail-detail-meta">
          <div>来自：{from}</div>
          {mail && <div>收件：{to}</div>}
          <div>时间：{fmtDate(date)}</div>
          {mail?.hasAttachment && (
            <div className="attachments">
              <div className="attach-title">📎 含 {mail.attachments?.length ?? 0} 个附件</div>
              <ul className="attach-list">
                {(mail.attachments ?? []).map((a, i) => (
                  <li key={i} className="attach-item">
                    <span className="attach-name">{a.filename}</span>
                    <span className="attach-meta">
                      {a.contentType} · {(a.sizeBytes / 1024).toFixed(1)} KB
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
          {mail && (
            <div className={`read-state ${mail.read ? 'read' : 'unread'}`}>
              {mail.read ? '已读' : '未读'}
            </div>
          )}
        </div>
        <pre className="mail-detail-body">{displayBody}</pre>
        {mail && (
          <div className="detail-actions">
            <button onClick={() => onToggleRead?.(mail)} className="detail-btn">
              {mail.read ? '标记未读' : '标记已读'}
            </button>
            <button onClick={() => onDelete?.(mail)} className="detail-btn danger">
              删除
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
