import { forwardRef, useState } from 'react'
import { Icon } from './Icon'

interface Props {
  onSearch: (q: string) => void
}

// forwardRef 暴露 input，供 App 通过全局快捷键 "/" 聚焦。
// 结构上把 input 与提交按钮拼成一个整体控件：容器负责边框与焦点环，
// 输入框自身去掉边框，视觉上不再出现"两段拼接"的割裂感。
const SearchBar = forwardRef<HTMLInputElement, Props>(({ onSearch }, ref) => {
  const [q, setQ] = useState('')

  return (
    <div className="search-bar">
      <span className="search-icon" aria-hidden="true">
        <Icon name="search" size={16} />
      </span>
      <input
        ref={ref}
        value={q}
        type="search"
        aria-label="搜索邮件"
        placeholder="搜索邮件主题、内容或发件人（按 / 聚焦）"
        onChange={(e) => setQ(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') onSearch(q)
        }}
      />
      {q && (
        <button
          type="button"
          className="search-clear"
          onClick={() => {
            setQ('')
            onSearch('')
          }}
          aria-label="清空搜索"
        >
          <Icon name="x" size={16} />
        </button>
      )}
      <button type="button" onClick={() => onSearch(q)}>
        搜索
      </button>
    </div>
  )
})

SearchBar.displayName = 'SearchBar'

export default SearchBar
