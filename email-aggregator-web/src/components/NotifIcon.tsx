import { Icon, type IconName } from './Icon'

// 通知类型 → 图标 + 文案 的统一映射（toast 与常驻通知面板共用，单一真相源）。
export const NOTIF_META: Record<string, { icon: IconName; label: string }> = {
  'new-mail': { icon: 'envelope-open', label: '新邮件' },
  'sync-state': { icon: 'sync', label: '同步' },
  'mail-updated': { icon: 'envelope', label: '已读更新' },
  'mail-deleted': { icon: 'trash', label: '邮件删除' },
  error: { icon: 'warning', label: '错误' },
}

// 渲染某类通知的图标 + 文案（带 notif-<kind> 修饰类，供左侧色条/语义着色）。
export function NotifIcon({ kind, size = 15 }: { kind: string; size?: number }) {
  const m = NOTIF_META[kind] ?? { icon: 'warning' as IconName, label: kind }
  return (
    <span className={`notif-icon notif-${kind}`}>
      <Icon name={m.icon} size={size} />
      <span className="notif-icon-label">{m.label}</span>
    </span>
  )
}
