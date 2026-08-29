interface Props {
  ok: boolean | null
}

export default function HealthBadge({ ok }: Props) {
  const text = ok === null ? '检测中…' : ok ? '后端在线' : '后端离线'
  const cls = ok === null ? 'badge pending' : ok ? 'badge ok' : 'badge err'
  return <span className={cls}>{text}</span>
}
