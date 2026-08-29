import { useEffect, useState, useCallback, useRef } from 'react'
import { health, listMails, searchMails, demoPush, listAccounts, getMail, setRead, deleteMail } from './api/client'
import type { CanonicalMail, SearchHit, NotificationPayload, AccountInfo } from './types'
import HealthBadge from './components/HealthBadge'
import SearchBar from './components/SearchBar'
import MailList from './components/MailList'
import AiChat from './components/AiChat'
import MailDetail from './components/MailDetail'
import NotificationFeed from './components/NotificationFeed'

const DEFAULT_ACCOUNT = 'acc_demo'
// 后端不可用时的兜底账户集合（未读数置 0），避免切换 UI 空白。
const FALLBACK_ACCOUNTS: AccountInfo[] = [
  { id: DEFAULT_ACCOUNT, unread: 0 },
  { id: 'acc_work', unread: 0 },
  { id: 'acc_personal', unread: 0 },
]

export default function App() {
  const [accountId, setAccountId] = useState(DEFAULT_ACCOUNT)
  const [accounts, setAccounts] = useState<AccountInfo[]>(FALLBACK_ACCOUNTS)
  const [healthOk, setHealthOk] = useState<boolean | null>(null)
  const [mails, setMails] = useState<CanonicalMail[]>([])
  const [hits, setHits] = useState<SearchHit[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // 实时推送相关：wsState 三态（open/closed/reconnecting）更诚实反映连接状况
  const [wsState, setWsState] = useState<'open' | 'closed' | 'reconnecting'>('closed')
  const [toasts, setToasts] = useState<NotificationPayload[]>([])
  const [notifications, setNotifications] = useState<NotificationPayload[]>([])
  const [selectedMail, setSelectedMail] = useState<CanonicalMail | null>(null)
  const [selectedHit, setSelectedHit] = useState<SearchHit | null>(null)
  const searchRef = useRef<HTMLInputElement>(null)

  // 当前账户权威未读数（来自 /api/accounts 注册表，与账户 chip 徽标一致），
  // 替代原先对所有 WS 事件 +1 且不清零的 session 计数器，避免"未读"语义失真。
  const currentUnread = accounts.find((a) => a.id === accountId)?.unread ?? 0

  const pushToast = useCallback((p: NotificationPayload) => {
    setToasts((t) => [...t, p])
    // 5 秒后自动消失
    window.setTimeout(() => {
      setToasts((t) => t.filter((x) => x !== p))
    }, 5000)
  }, [])

  const loadMails = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const resp = await listMails(accountId)
      setMails(resp.mails)
      setHits(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [accountId])

  const doSearch = useCallback(async (q: string) => {
    if (!q.trim()) {
      setHits(null)
      return
    }
    setLoading(true)
    setError(null)
    try {
      const resp = await searchMails(accountId, q)
      setHits(resp.hits)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [accountId])

  // 刷新账户注册表（含未读数），保持切换 chips 与徽标与服务端一致。
  const refreshAccounts = useCallback(() => {
    listAccounts()
      .then((r) => {
        if (r.accounts && r.accounts.length > 0) setAccounts(r.accounts)
      })
      .catch(() => {
        /* 后端未暴露 /api/accounts 时沿用兜底集合 */
      })
  }, [])

  // 打开邮件：先按 ID 拉取服务端权威资源再渲染详情，使模态为真实数据而非列表副本；
  // 打开即标记已读（乐观更新列表 + 服务端权威），失败时回退到列表/检索传入的对象。
  const openMail = useCallback(async (mail: CanonicalMail) => {
    try {
      const fetched = await getMail(mail.accountId, mail.id)
      if (!fetched.read) {
        setRead(mail.accountId, mail.id, true).catch(() => {})
        fetched.read = true
        setMails((ms) => ms.map((m) => (m.id === mail.id ? { ...m, read: true } : m)))
      }
      setSelectedMail(fetched)
    } catch {
      setSelectedMail(mail)
    }
  }, [])

  const openHit = useCallback(async (hit: SearchHit) => {
    setSelectedHit(hit)
    try {
      const fetched = await getMail(hit.accountId, hit.idempotencyKey)
      if (!fetched.read) {
        setRead(hit.accountId, hit.idempotencyKey, true).catch(() => {})
        fetched.read = true
      }
      setSelectedMail(fetched)
    } catch {
      /* 保留 selectedHit 预览兜底 */
    }
  }, [])

  // 详情内切换已读/未读（乐观更新 + 服务端权威 + 刷新徽标）。
  const toggleRead = useCallback(
    async (mail: CanonicalMail) => {
      const next = !mail.read
      setSelectedMail((sm) => (sm ? { ...sm, read: next } : sm))
      setMails((ms) => ms.map((m) => (m.id === mail.id ? { ...m, read: next } : m)))
      try {
        await setRead(mail.accountId, mail.id, next)
      } catch {
        /* 保留乐观结果 */
      }
      refreshAccounts()
    },
    [refreshAccounts],
  )

  // 详情内删除邮件（本地即时移除 + 服务端权威 + 刷新徽标；WS 事件兜底）。
  const removeMail = useCallback(
    async (mail: CanonicalMail) => {
      setMails((ms) => ms.filter((m) => m.id !== mail.id))
      setSelectedMail(null)
      setSelectedHit(null)
      try {
        await deleteMail(mail.accountId, mail.id)
      } catch {
        /* 保留本地移除结果 */
      }
      refreshAccounts()
    },
    [refreshAccounts],
  )

  // 首屏：健康检查 + 拉取账户注册表 + 拉取收件箱
  useEffect(() => {
    health()
      .then((r) => setHealthOk(r.ok))
      .catch(() => setHealthOk(false))
    refreshAccounts()
    loadMails()
  }, [loadMails, refreshAccounts])

  // WebSocket 实时推送：连接 /ws?accountId=，监听 new-mail 通知，断线自动重连。
  useEffect(() => {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const url = `${proto}://${location.host}/ws?accountId=${encodeURIComponent(accountId)}`
    let ws: WebSocket | null = null
    let retryTimer: number | undefined

    const connect = () => {
      ws = new WebSocket(url)
      ws.onopen = () => setWsState('open')
      ws.onclose = () => {
        setWsState('reconnecting')
        retryTimer = window.setTimeout(connect, 2000)
      }
      ws.onerror = () => ws?.close()
      ws.onmessage = (ev) => {
        try {
          const p = JSON.parse(ev.data) as NotificationPayload
          pushToast(p)
          setNotifications((n) => [p, ...n].slice(0, 20))
          // 事件驱动实时刷新当前账户列表
          if (p.accountId === accountId) {
            if (p.kind === 'mail-updated' && p.mailId) {
              setMails((ms) => ms.map((m) => (m.id === p.mailId ? { ...m, read: p.read } : m)))
            } else if (p.kind === 'mail-deleted' && p.mailId) {
              setMails((ms) => ms.filter((m) => m.id !== p.mailId))
              setSelectedMail((sm) => (sm && sm.id === p.mailId ? null : sm))
              setSelectedHit((sh) => (sh && sh.idempotencyKey === p.mailId ? null : sh))
            }
          }
          // 任何事件都可能改变未读数，刷新账户徽标
          refreshAccounts()
        } catch {
          /* 忽略非法帧 */
        }
      }
    }
    connect()

    return () => {
      if (retryTimer) window.clearTimeout(retryTimer)
      ws?.close()
    }
  }, [accountId, pushToast, refreshAccounts])

  // 全局键盘快捷键：非输入框聚焦时按 "/" 聚焦搜索框。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null
      const typing = !!el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA')
      if (e.key === '/' && !typing) {
        e.preventDefault()
        searchRef.current?.focus()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  // 模拟收信：触发后端推送一封演示新邮件，并经 WS 实时到达前端。
  const simulate = useCallback(async () => {
    setError(null)
    try {
      await demoPush(accountId)
      // WS 通知到达后刷新列表/徽标以呈现新邮件
      window.setTimeout(() => {
        loadMails()
        refreshAccounts()
      }, 400)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [accountId, loadMails, refreshAccounts])

  // 清除检索：返回收件箱视图（保留已加载邮件，仅收起命中列表）
  const clearSearch = useCallback(() => setHits(null), [])

  return (
    <div className="app">
      <header className="app-header">
        <h1>邮箱聚合平台</h1>
        <div className="header-status">
          <HealthBadge ok={healthOk} />
          <span className={`ws-badge ${wsState === 'open' ? 'on' : wsState === 'reconnecting' ? 'reconnecting' : 'off'}`}>
            {wsState === 'open' ? '● 实时已连接' : wsState === 'reconnecting' ? '↻ 重连中…' : '○ 实时未连接'}
          </span>
          {currentUnread > 0 && <span className="unread-badge">{currentUnread}</span>}
        </div>
      </header>

      <section className="toolbar">
        <div className="account-switch" role="tablist" aria-label="账户切换">
          <span className="account-switch-label">账户</span>
          {accounts.map((acc) => (
            <button
              key={acc.id}
              role="tab"
              aria-selected={acc.id === accountId}
              className={`account-chip ${acc.id === accountId ? 'active' : ''}`}
              onClick={() => setAccountId(acc.id)}
            >
              {acc.id}
              {acc.unread > 0 && <span className="chip-badge">{acc.unread}</span>}
            </button>
          ))}
        </div>
        <button onClick={loadMails} disabled={loading}>
          {loading ? '加载中…' : '刷新邮件'}
        </button>
        <button onClick={simulate} disabled={loading} className="simulate-btn">
          模拟收信
        </button>
        <SearchBar ref={searchRef} onSearch={doSearch} />
        {hits && (
          <button onClick={clearSearch} className="clear-search-btn">
            清除检索
          </button>
        )}
      </section>

      {error && <div className="error">错误：{error}</div>}

      {/* 实时通知 toast 区 */}
      <div className="toast-zone">
        {toasts.map((t, i) => (
          <div key={`${t.ts}-${i}`} className={`toast toast-${t.kind}`}>
            <strong>
              {t.kind === 'new-mail' ? '📬 新邮件' : t.kind === 'sync-state' ? '🔄 同步' : '⚠️ 错误'}
            </strong>
            <span>{t.preview || t.accountId}</span>
          </div>
        ))}
      </div>

      <main className="content">
        <section className="mail-pane">
          <h2>{hits ? '检索结果' : `收件箱 · ${accountId}`}</h2>
          {hits ? (
            <MailList hits={hits} onSelectHit={openHit} />
          ) : (
            <MailList mails={mails} loading={loading} onSelectMail={openMail} />
          )}
        </section>
        <aside className="ai-pane">
          <NotificationFeed items={notifications} />
          <AiChat accountId={accountId} />
        </aside>
      </main>

      <MailDetail
        mail={selectedMail}
        hit={selectedHit}
        onClose={() => {
          setSelectedMail(null)
          setSelectedHit(null)
        }}
        onToggleRead={toggleRead}
        onDelete={removeMail}
      />
    </div>
  )
}
