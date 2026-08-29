import { useEffect, useState, useCallback, useRef } from 'react'
import {
  health,
  listMails,
  searchMails,
  demoPush,
  listAccounts,
  getMail,
  setRead,
  deleteMail,
  createAccount,
  updateAccountStatus,
  deleteAccount as apiDeleteAccount,
} from './api/client'
import type { CanonicalMail, SearchHit, NotificationPayload, AccountInfo, AccountStatus, Provider } from './types'
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

// toast 文案映射：覆盖全部 NotifyKind（new-mail / sync-state / mail-updated /
// mail-deleted / error），未知 kind 回退显示 kind 本身，避免把正常事件误报成"错误"。
const TOAST_LABELS: Record<string, string> = {
  'new-mail': '📬 新邮件',
  'sync-state': '🔄 同步',
  'mail-updated': '📩 已读更新',
  'mail-deleted': '🗑️ 邮件删除',
  error: '⚠️ 错误',
}

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

  // 连接/账户服务：添加账户表单状态 + 操作中标志（防重复提交）
  const [addingAccount, setAddingAccount] = useState(false)
  const [accForm, setAccForm] = useState({ id: '', provider: 'imap' as Provider, email: '', displayName: '' })
  const [accountBusy, setAccountBusy] = useState(false)

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

  // 新增账户：提交到连接/账户服务注册表，成功后刷新列表并切换。
  const addAccount = useCallback(async () => {
    if (!accForm.id.trim() || !accForm.email.trim()) {
      setError('账户 ID 与邮箱必填')
      return
    }
    setAccountBusy(true)
    setError(null)
    try {
      await createAccount({
        id: accForm.id.trim(),
        provider: accForm.provider,
        email: accForm.email.trim(),
        displayName: accForm.displayName.trim() || undefined,
        status: 'active',
        syncFolder: 'INBOX',
      })
      setAccForm({ id: '', provider: 'imap', email: '', displayName: '' })
      setAddingAccount(false)
      setAccountId(accForm.id.trim())
      refreshAccounts()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setAccountBusy(false)
    }
  }, [accForm, refreshAccounts])

  // 暂停 / 恢复：仅对注册表已存在的账户生效；被暂停账户将不再参与同步。
  const togglePause = useCallback(
    async (acc: AccountInfo) => {
      const next: AccountStatus = acc.status === 'paused' ? 'active' : 'paused'
      setAccountBusy(true)
      setError(null)
      try {
        await updateAccountStatus(acc.id, next)
        refreshAccounts()
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setAccountBusy(false)
      }
    },
    [refreshAccounts],
  )

  // 删除账户：从注册表移除；若删除的是当前账户，切回默认账户。
  const removeAccount = useCallback(
    async (acc: AccountInfo) => {
      setAccountBusy(true)
      setError(null)
      try {
        await apiDeleteAccount(acc.id)
        if (acc.id === accountId) setAccountId(DEFAULT_ACCOUNT)
        refreshAccounts()
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setAccountBusy(false)
      }
    },
    [accountId, refreshAccounts],
  )

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
              className={`account-chip ${acc.id === accountId ? 'active' : ''} ${acc.status && acc.status !== 'active' ? `status-${acc.status}` : ''}`}
              onClick={() => setAccountId(acc.id)}
              title={acc.email ? `${acc.email} · ${acc.provider ?? ''}` : undefined}
            >
              {acc.id}
              {acc.status && acc.status !== 'active' && (
                <span className="chip-status">{acc.status === 'paused' ? '暂停' : '异常'}</span>
              )}
              {acc.unread > 0 && <span className="chip-badge">{acc.unread}</span>}
            </button>
          ))}
        </div>
        <button
          onClick={() => setAddingAccount((v) => !v)}
          className="add-account-btn"
          aria-expanded={addingAccount}
        >
          {addingAccount ? '收起' : '+ 添加账户'}
        </button>
        {accounts.find((a) => a.id === accountId) && (
          <div className="account-actions" aria-label="当前账户操作">
            <button onClick={() => togglePause(accounts.find((a) => a.id === accountId)!)} disabled={accountBusy}>
              {accounts.find((a) => a.id === accountId)!.status === 'paused' ? '恢复同步' : '暂停同步'}
            </button>
            <button
              onClick={() => removeAccount(accounts.find((a) => a.id === accountId)!)}
              disabled={accountBusy}
              className="danger-btn"
            >
              删除账户
            </button>
          </div>
        )}
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

      {addingAccount && (
        <section className="account-form" aria-label="添加账户">
          <input
            placeholder="账户 ID（如 acc_new）"
            value={accForm.id}
            onChange={(e) => setAccForm((f) => ({ ...f, id: e.target.value }))}
          />
          <select
            value={accForm.provider}
            onChange={(e) => setAccForm((f) => ({ ...f, provider: e.target.value as Provider }))}
          >
            <option value="imap">IMAP</option>
            <option value="pop3">POP3</option>
            <option value="gmail">Gmail</option>
            <option value="exchange">Exchange</option>
          </select>
          <input
            placeholder="邮箱（如 user@example.com）"
            value={accForm.email}
            onChange={(e) => setAccForm((f) => ({ ...f, email: e.target.value }))}
          />
          <input
            placeholder="显示名（可选）"
            value={accForm.displayName}
            onChange={(e) => setAccForm((f) => ({ ...f, displayName: e.target.value }))}
          />
          <button onClick={addAccount} disabled={accountBusy}>
            {accountBusy ? '提交中…' : '创建'}
          </button>
        </section>
      )}

      {error && <div className="error">错误：{error}</div>}

      {/* 实时通知 toast 区 */}
      <div className="toast-zone">
        {toasts.map((t, i) => (
          <div key={`${t.ts}-${i}`} className={`toast toast-${t.kind}`}>
            <strong>
              {TOAST_LABELS[t.kind] ?? t.kind}
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
