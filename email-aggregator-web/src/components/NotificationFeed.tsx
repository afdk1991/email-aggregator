import type { NotificationPayload } from '../types'
import { Icon } from './Icon'
import { NOTIF_META } from './NotifIcon'

interface Props {
  items: NotificationPayload[]
}

// 实时通知常驻面板：持续累积 WS 收到的事件（与瞬时 toast 并存），上限 20 条。
// 图标+文案映射复用 NotifIcon.NOTIF_META 单一真相源，避免与 toast 定义不一致。
export default function NotificationFeed({ items }: Props) {
  return (
    <div className="notif-feed">
      <h3>实时通知</h3>
      {items.length === 0 ? (
        <p className="empty">暂无实时通知</p>
      ) : (
        <ul className="notif-list">
          {items.map((n, i) => {
            const meta = NOTIF_META[n.kind]
            return (
              <li key={`${n.ts}-${i}`} className={`notif-item notif-${n.kind}`}>
                <span className="notif-kind">
                  {meta ? <Icon name={meta.icon} size={14} /> : null}
                  {meta ? meta.label : n.kind}
                </span>
                <span className="notif-text">{n.preview || n.accountId}</span>
                <span className="notif-time">{new Date(n.ts * 1000).toLocaleTimeString()}</span>
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}
