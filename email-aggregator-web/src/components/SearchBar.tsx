import { forwardRef, useState } from 'react'
import { Icon } from './Icon'

interface Props {
  onSearch: (q: string) => void
}

// forwardRef 暴露 input，供 App 通过全局快捷键 "/" 聚焦。
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
        aria-label="搜索邮件"
        placeholder="搜索邮件内容/主题/发件人 (按 / 聚焦)"
        onChange={(e) => setQ(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') onSearch(q)
        }}
      />
      {q && (
        <button
          type="button"
          className="search-clear"
          onClick={() => { setQ(''); onSearch('') }}
          aria-label="清空搜索"
        >
          <Icon name="x" size={18} />
        </button>
      )}
      <button onClick={() => onSearch(q)}>搜索</button>
    </div>
  )
})

SearchBar.displayName = 'SearchBar'

export default SearchBar
