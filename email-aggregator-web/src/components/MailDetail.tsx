import { useEffect, useRef } from 'react'
import type { CanonicalMail, SearchHit } from '../types'
import { Icon } from './Icon'

interface Props {
  mail?: CanonicalMail | null
  hit?: SearchHit | null
  onClose: () => void
  onToggleRead?: (mail: CanonicalMail) => void
  onDelete?: (mail: CanonicalMail) => void
}

function fmtDate(ts: number): string {
  if (!ts) return ''
  return new Date(ts * 1000).toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

// 邮件详情模态：点击列表项打开，展示完整邮件内容。
// mail 优先（含正文/收件人/附件），命中项 hit 仅含预览。
// 仅当存在权威 mail 时提供「标记已读/未读」「删除」操作。
// 结构：固定头部（主题+元信息）→ 可滚动正文 → 固定底部操作条。
export default function MailDetail({ mail, hit, onClose, onToggleRead, onDelete }: Props) {
  const closeRef = useRef<HTMLButtonElement>(null)
  const modalRef = useRef<HTMLDivElement>(null)

  // Esc 关闭模态 + Tab 焦点陷阱（仅在打开时注册监听，避免全局常驻）
  useEffect(() => {
    if (!mail && !hit) return
    // 打开时聚焦关闭按钮（屏幕阅读器与键盘用户起点）
    closeRef.current?.focus()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose()
        return
      }
      // 焦点陷阱：Tab / Shift+Tab 在模态内循环，禁止逃出到背景
      if (e.key === 'Tab' && modalRef.current) {
        const focusables = modalRef.current.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        )
        const list = Array.from(focusables).filter((el) => !el.hasAttribute('disabled'))
        if (list.length === 0) {
          e.preventDefault()
          return
        }
        const first = list[0]
        const last = list[list.length - 1]
        const active = document.activeElement as HTMLElement | null
        if (e.shiftKey && active === first) {
          e.preventDefault()
          last.focus()
        } else if (!e.shiftKey && active === last) {
          e.preventDefault()
          first.focus()
        } else if (!modalRef.current.contains(active)) {
          // 焦点意外跑出（如初始态），拉回首个
          e.preventDefault()
          first.focus()
        }
      }
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
    <div className="modal-overlay" onClick={onClose} role="presentation">
      <div
        ref={modalRef}
        className="modal"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-labelledby="mail-detail-subject"
      >
        <button ref={closeRef} className="modal-close" onClick={onClose} aria-label="关闭详情对话框">
          <Icon name="x" size={20} />
        </button>

        {/* 固定头部：主题 + 元信息（滚动正文时保持可见） */}
        <div className="modal-head">
          <h3 id="mail-detail-subject" className="mail-detail-subject">
            {subject}
          </h3>
          <dl className="mail-detail-meta">
            <dt>发件人</dt>
            <dd>{from || '—'}</dd>
            {mail && (
              <>
                <dt>收件人</dt>
                <dd>{to}</dd>
              </>
            )}
            <dt>时间</dt>
            <dd>{fmtDate(date) || '—'}</dd>
            {mail && (
              <>
                <dt>状态</dt>
                <dd>
                  <span className={`read-state ${mail.read ? 'read' : 'unread'}`}>
                    {mail.read ? '已读' : '未读'}
                  </span>
                </dd>
              </>
            )}
            {mail?.hasAttachment && (
              <div className="attachments">
                <div className="attach-title">
                  <Icon name="paperclip" size={14} /> 含 {mail.attachments?.length ?? 0} 个附件
                </div>
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
          </dl>
        </div>

        {/* 可滚动正文 */}
        <div className="modal-body">
          <pre className="mail-detail-body">{displayBody}</pre>
        </div>

        {/* 固定底部操作条：低频/破坏性操作靠右，与阅读动线分离 */}
        {mail && (
          <div className="modal-foot">
            <button onClick={() => onToggleRead?.(mail)} className="btn btn--secondary">
              <Icon name={mail.read ? 'envelope' : 'check'} size={15} />
              {mail.read ? '标记未读' : '标记已读'}
            </button>
            <button
              onClick={() => onDelete?.(mail)}
              className="btn btn--danger"
              style={{ marginLeft: 'auto' }}
            >
              <Icon name="trash" size={15} />
              删除
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
