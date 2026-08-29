import type { NotificationPayload } from '../types'

interface Props {
  items: NotificationPayload[]
}

const KIND_LABEL: Record<string, string> = {
  'new-mail': '📬 新邮件',
  'sync-state': '🔄 同步',
  'mail-updated': '📩 已读更新',
  'mail-deleted': '🗑️ 邮件删除',
  error: '⚠️ 错误',
}

// 实时通知常驻面板：持续累积 WS 收到的事件（与瞬时 toast 并存），上限 20 条。
export default function NotificationFeed({ items }: Props) {
  return (
    <div className="notif-feed">
      <h3>实时通知</h3>
      {items.length === 0 ? (
        <p className="empty">暂无实时通知</p>
      ) : (
        <ul className="notif-list">
          {items.map((n, i) => (
            <li key={`${n.ts}-${i}`} className={`notif-item notif-${n.kind}`}>
              <span className="notif-kind">{KIND_LABEL[n.kind] ?? n.kind}</span>
              <span className="notif-text">{n.preview || n.accountId}</span>
              <span className="notif-time">{new Date(n.ts * 1000).toLocaleTimeString()}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
