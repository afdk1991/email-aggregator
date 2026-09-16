interface Props {
  ok: boolean | null
}

// 后端健康状态徽标：三态各有独立语义色（检测中=警告、在线=成功、离线=错误），
// 并配状态点作为非颜色冗余信号。
export default function HealthBadge({ ok }: Props) {
  const text = ok === null ? '检测中' : ok ? '后端在线' : '后端离线'
  const cls = ok === null ? 'pending' : ok ? 'ok' : 'err'
  return (
    <span className={`badge ${cls}`} role="status">
      <span className="status-dot" aria-hidden="true" />
      {text}
    </span>
  )
}
