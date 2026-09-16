import { useEffect, useState, useCallback, useRef } from 'react'
import { Icon } from './components/Icon'
import { NOTIF_META } from './components/NotifIcon'
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
  saveAccountCredentials,
  syncAccount,
} from './api/client'
import type { CanonicalMail, SearchHit, NotificationPayload, AccountInfo, AccountStatus, Provider } from './types'
import HealthBadge from './components/HealthBadge'
import SearchBar from './components/SearchBar'
import MailList from './components/MailList'
import AiChat from './components/AiChat'
import MailDetail from './components/MailDetail'
import NotificationFeed from './components/NotificationFeed'

// 初始选中的账户：演示形态对齐后端种子账户，生产形态用中性 ID（库为空时不会命中任何账户）。
const DEFAULT_ACCOUNT = __DEMO_MODE__ ? 'acc_demo' : 'default'
// 已下线的演示账户：后端已剔除且拒绝重建，前端兜底列表同步移除，避免"删了又出现"。
const RETIRED_ACCOUNT_IDS = [
  'acc_demo3',
  'acc_demo32',
  'acc_personal1',
  'acc_personal11',
  'acc_work1',
  'acc_work11',
]
// 后端不可用时的兜底账户集合（未读数置 0），避免切换 UI 空白。
// 生产形态（DEMO_MODE=false）不预置任何账户——宁可空列表，也不展示假账户。
const FALLBACK_ACCOUNTS: AccountInfo[] = __DEMO_MODE__
  ? [
      { id: DEFAULT_ACCOUNT, unread: 0 },
      { id: 'acc_work', unread: 0 },
      { id: 'acc_personal', unread: 0 },
    ]
  : []

// toast/通知图标+文案统一映射来自 NotifIcon.NOTIF_META（覆盖全部 NotifyKind，
// 未知 kind 回退显示 kind 本身，避免把正常事件误报成"错误"）。

export default function App() {
  const [accountId, setAccountId] = useState(DEFAULT_ACCOUNT)
  const [accounts, setAccounts] = useState<AccountInfo[]>(FALLBACK_ACCOUNTS)
  const [healthOk, setHealthOk] = useState<boolean | null>(null)
  const [mails, setMails] = useState<CanonicalMail[]>([])
  const [hits, setHits] = useState<SearchHit[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // 主题状态：初始值由 index.html 内联脚本根据 localStorage/prefers-color-scheme 设置
  const [theme, setTheme] = useState<'light' | 'dark'>(() => {
    if (typeof document !== 'undefined') {
      return document.documentElement.getAttribute('data-theme') === 'dark' ? 'dark' : 'light'
    }
    return 'light'
  })
  const toggleTheme = useCallback(() => {
    setTheme((prev) => {
      const next = prev === 'dark' ? 'light' : 'dark'
      document.documentElement.setAttribute('data-theme', next)
      localStorage.setItem('theme', next)
      return next
    })
  }, [])

  // 实时推送相关：wsState 三态（open/closed/reconnecting）更诚实反映连接状况
  const [wsState, setWsState] = useState<'open' | 'closed' | 'reconnecting'>('closed')
  const [toasts, setToasts] = useState<NotificationPayload[]>([])
  const [notifications, setNotifications] = useState<NotificationPayload[]>([])
  const [selectedMail, setSelectedMail] = useState<CanonicalMail | null>(null)
  const [selectedHit, setSelectedHit] = useState<SearchHit | null>(null)
  const searchRef = useRef<HTMLInputElement>(null)

  // 连接/账户服务：添加账户表单状态 + 操作中标志（防重复提交）
  const [addingAccount, setAddingAccount] = useState(false)
  const [accForm, setAccForm] = useState({ id: '', provider: 'imap' as Provider, email: '', displayName: '', serverHost: '' })
  const [accountBusy, setAccountBusy] = useState(false)

  // 真实邮箱凭据录入（授权码/密码 → 服务端 KMS 信封加密）
  const [credForm, setCredForm] = useState<{ open: boolean; username: string; password: string }>({
    open: false,
    username: '',
    password: '',
  })

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

  // 刷新账户注册表（含未读数），保持侧边栏与徽标跟服务端一致。
  // 过滤已下线账户：即便某个后端实例暂未升级、仍返回旧数据，也不会再显示。
  const refreshAccounts = useCallback(() => {
    listAccounts()
      .then((r) => {
        if (r.accounts && r.accounts.length > 0) {
          setAccounts(r.accounts.filter((a) => !RETIRED_ACCOUNT_IDS.includes(a.id)))
        }
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
      if (!window.confirm(`确定删除邮件「${mail.subject || '(无主题)'}」？此操作不可撤销。`)) return
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
    let disposed = false
    let everOpened = false
    let attempts = 0

    const connect = () => {
      ws = new WebSocket(url)
      ws.onopen = () => {
        everOpened = true
        attempts = 0
        setWsState('open')
      }
      ws.onclose = () => {
        // 卸载或切换账户触发的主动关闭不再重连
        if (disposed) return
        attempts += 1
        // 曾连上后掉线才提示“重连中”；平台不提供 WS、从未连上时保持中性“未连接”，避免无限“重连中”刷屏
        setWsState(everOpened ? 'reconnecting' : 'closed')
        // 指数退避并封顶：掉线重连最快 2s（上限 15s）；从未连上则放慢到最长 30s 静默探活，平台恢复即自动连上
        const delay = Math.min(2000 * 2 ** (attempts - 1), everOpened ? 15000 : 30000)
        retryTimer = window.setTimeout(connect, delay)
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
      disposed = true
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
        serverHost: accForm.serverHost.trim() || undefined,
      })
      setAccForm({ id: '', provider: 'imap', email: '', displayName: '', serverHost: '' })
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
      if (!window.confirm(`确定删除账户「${acc.id}」？其邮件数据将被清除且不可恢复。`)) return
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

  // 返回首页：清除检索/详情/表单等临时状态并回到默认账户收件箱。
  // 本项目是单页应用（无路由），"首页"即这一初始视图状态。
  const goHome = useCallback(() => {
    setHits(null)
    setSelectedMail(null)
    setSelectedHit(null)
    setAddingAccount(false)
    setCredForm({ open: false, username: '', password: '' })
    setError(null)
    if (accountId !== DEFAULT_ACCOUNT) {
      setAccountId(DEFAULT_ACCOUNT) // 触发 accountId 依赖 effect 重新拉列表
    } else {
      loadMails()
    }
  }, [accountId, loadMails])

  // 保存账户凭据（授权码/密码）：服务端信封加密后存 credentialsRef，用于真实同步。
  const saveCreds = useCallback(
    async (acc: AccountInfo) => {
      if (!credForm.password.trim()) {
        setError('授权码/密码必填')
        return
      }
      setAccountBusy(true)
      setError(null)
      try {
        await saveAccountCredentials(acc.id, credForm.username.trim() || acc.email || '', credForm.password)
        setCredForm({ open: false, username: '', password: '' })
        refreshAccounts()
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setAccountBusy(false)
      }
    },
    [credForm, refreshAccounts],
  )

  // 立即同步：后台触发一次账户真实同步（连接器 → mail-ingested → 摄取管线）。
  const triggerSync = useCallback(
    async (acc: AccountInfo) => {
      setError(null)
      try {
        await syncAccount(acc.id)
        // 轮询同步状态：running → ok/error
        let tries = 0
        const poll = window.setInterval(async () => {
          tries++
          try {
            const resp = await listAccounts()
            const cur = resp.accounts.find((a) => a.id === acc.id)
            if (cur?.syncing) return
            window.clearInterval(poll)
            if (cur?.lastSyncError) {
              setError(`同步失败：${cur.lastSyncError}`)
            } else {
              refreshAccounts()
              loadMails()
            }
          } catch {
            if (tries > 60) window.clearInterval(poll)
          }
        }, 1000)
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e))
      }
    },
    [refreshAccounts, loadMails],
  )

  return (
    <div className="app">
      {/* 跳过导航直达内容（WCAG 2.4.1） */}
      <a href="#main-content" className="skip-link">跳至主内容</a>
      <header className="app-header">
        {/* logo 即回首页入口：单页应用无路由，重置为初始视图状态 */}
        <a
          href="/"
          className="logo"
          onClick={(e) => {
            e.preventDefault()
            goHome()
          }}
          aria-label="返回首页"
          title="返回首页"
        >
          <span className="logo-mark" aria-hidden="true">
            <Icon name="envelope" size={18} />
          </span>
          <span className="logo-text">邮箱聚合平台</span>
        </a>
        <div className="header-status">
          {/* 主操作：全页唯一的实色按钮，明确"添加账户"是第一优先级动作 */}
          <button
            onClick={() => setAddingAccount((v) => !v)}
            className="btn btn--primary btn--md add-account-btn"
            aria-expanded={addingAccount}
          >
            <Icon name={addingAccount ? 'x' : 'plus'} size={15} />
            {addingAccount ? '收起' : '添加账户'}
          </button>
          <button
            type="button"
            className="btn btn--secondary icon-btn theme-toggle"
            onClick={toggleTheme}
            aria-label={theme === 'dark' ? '切换到浅色模式' : '切换到深色模式'}
            title={theme === 'dark' ? '切换到浅色模式' : '切换到深色模式'}
          >
            <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={17} />
          </button>
          {/* 状态组：把多个状态徽标聚合为一个视觉整体，降低顶栏噪声 */}
          <div className="status-group" role="group" aria-label="服务状态">
            <HealthBadge ok={healthOk} />
            <span
              className={`badge ${wsState === 'open' ? 'ok' : wsState === 'reconnecting' ? 'pending' : 'idle'}`}
            >
              <span className="status-dot" aria-hidden="true" />
              {wsState === 'open' ? '实时已连接' : wsState === 'reconnecting' ? '重连中' : '实时未连接'}
            </span>
            {currentUnread > 0 && (
              <span className="count-badge" aria-label={`当前账户 ${currentUnread} 封未读`}>
                {currentUnread > 99 ? '99+' : currentUnread}
              </span>
            )}
          </div>
        </div>
      </header>

      <div className="app-body">
        <aside className="sidebar" aria-label="账户导航">
          <div className="group-title">账户</div>
          <nav className="account-nav" role="group" aria-label="账户切换">
            {accounts.length === 0 && (
              <p className="sidebar-empty">暂无账户，点击右上角「添加账户」开始</p>
            )}
            {accounts.map((acc) => (
              <button
                key={acc.id}
                aria-pressed={acc.id === accountId}
                className={`account-chip ${acc.id === accountId ? 'active' : ''} ${acc.status && acc.status !== 'active' ? `status-${acc.status}` : ''}`}
                onClick={() => setAccountId(acc.id)}
                title={acc.email ? `${acc.email} · ${acc.provider ?? ''}` : acc.id}
              >
                <Icon name={acc.id === accountId ? 'envelope-open' : 'envelope'} size={15} />
                <span className="chip-id">{acc.displayName || acc.id}</span>
                {acc.status && acc.status !== 'active' && (
                  <span className="chip-status">{acc.status === 'paused' ? '暂停' : '异常'}</span>
                )}
                {acc.unread > 0 && (
                  <span className="chip-badge">{acc.unread > 99 ? '99+' : acc.unread}</span>
                )}
              </button>
            ))}
          </nav>

          {(() => {
            const cur = accounts.find((a) => a.id === accountId)
            if (!cur) return null
            return (
              <div className="sidebar-actions">
                <div className="sidebar-actions-title" title={cur.email || cur.id}>
                  {cur.displayName || cur.id}
                </div>
                <div className="account-actions" aria-label="当前账户操作">
                  <button
                    onClick={() => triggerSync(cur)}
                    disabled={accountBusy || !!cur.syncing}
                    className="sync-btn"
                  >
                    <Icon name="sync" size={14} />
                    {cur.syncing ? '同步中…' : '立即同步'}
                  </button>
                  <button
                    onClick={() =>
                      setCredForm((f) => ({
                        open: !f.open,
                        username: f.open ? '' : (cur.email ?? ''),
                        password: '',
                      }))
                    }
                    disabled={accountBusy}
                  >
                    <Icon name="check" size={14} />
                    {credForm.open ? '收起凭据' : '设置凭据'}
                  </button>
                  <button onClick={() => togglePause(cur)} disabled={accountBusy}>
                    <Icon name={cur.status === 'paused' ? 'play' : 'pause'} size={14} />
                    {cur.status === 'paused' ? '恢复同步' : '暂停同步'}
                  </button>
                  <button
                    onClick={() => removeAccount(cur)}
                    disabled={accountBusy}
                    className="danger-btn"
                  >
                    <Icon name="trash" size={14} />
                    删除账户
                  </button>
                </div>
                {credForm.open && (
                  <div className="cred-form" aria-label="设置凭据">
                    <label className="field-label" htmlFor="cred-user">
                      用户名（授权码登录账号，缺省用邮箱）
                    </label>
                    <input
                      id="cred-user"
                      placeholder="用户名"
                      value={credForm.username}
                      onChange={(e) => setCredForm((f) => ({ ...f, username: e.target.value }))}
                    />
                    <label className="field-label" htmlFor="cred-pass">
                      授权码 / 密码
                    </label>
                    <input
                      id="cred-pass"
                      type="password"
                      placeholder="授权码 / 密码"
                      value={credForm.password}
                      onChange={(e) => setCredForm((f) => ({ ...f, password: e.target.value }))}
                    />
                    <button onClick={() => saveCreds(cur)} disabled={accountBusy} className="btn btn--primary">
                      保存凭据
                    </button>
                  </div>
                )}
                {cur.lastSyncError && !cur.syncing && (
                  <span className="sync-error" title={cur.lastSyncError}>
                    <Icon name="warning" size={14} /> 上次同步失败
                  </span>
                )}
              </div>
            )
          })()}
        </aside>

        <div className="main-col">
          <section className="toolbar" aria-label="邮件操作">
            <button onClick={loadMails} disabled={loading} className="btn btn--secondary">
              {loading ? (
                <>
                  <Icon name="sync" size={15} />
                  加载中…
                </>
              ) : (
                <>
                  <Icon name="sync" size={15} />
                  刷新邮件
                </>
              )}
            </button>
            {hits && (
              <button onClick={clearSearch} className="btn btn--outline">
                <Icon name="x" size={15} />
                清除检索
              </button>
            )}
            {__DEMO_MODE__ && (
              <button
                onClick={async () => {
                  // 演示入口：触发后端推送一封演示新邮件，并经 WS 实时到达前端。
                  // 生产构建下整块被折叠移除，不会出现在产物里。
                  setError(null)
                  try {
                    await demoPush(accountId)
                    window.setTimeout(() => {
                      loadMails()
                      refreshAccounts()
                    }, 400)
                  } catch (e) {
                    setError(e instanceof Error ? e.message : String(e))
                  }
                }}
                disabled={loading}
                className="btn btn--outline simulate-btn"
              >
                模拟收信
              </button>
            )}
            <SearchBar ref={searchRef} onSearch={doSearch} />
          </section>

      {addingAccount && (
        <section className="account-form" aria-label="添加账户">
          <div className="account-form-head">
            <h2>添加账户</h2>
            <p className="account-form-hint">
              填写账户标识与邮箱地址即可创建；服务器地址与凭据可在创建后于侧栏补充。
            </p>
          </div>
          <div className="account-form-fields">
            <div className="field">
              <label htmlFor="acc-id">账户 ID</label>
              <input
                id="acc-id"
                placeholder="如 acc_new"
                value={accForm.id}
                onChange={(e) => setAccForm((f) => ({ ...f, id: e.target.value }))}
              />
            </div>
            <div className="field">
              <label htmlFor="acc-provider">协议</label>
              <select
                id="acc-provider"
                value={accForm.provider}
                onChange={(e) => setAccForm((f) => ({ ...f, provider: e.target.value as Provider }))}
              >
                <option value="imap">IMAP</option>
                <option value="pop3">POP3</option>
                <option value="gmail">Gmail</option>
                <option value="exchange">Exchange (EWS)</option>
                <option value="graph">Microsoft 365 (Graph)</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="acc-email">邮箱地址</label>
              <input
                id="acc-email"
                placeholder="user@example.com"
                value={accForm.email}
                onChange={(e) => setAccForm((f) => ({ ...f, email: e.target.value }))}
              />
            </div>
            <div className="field">
              <label htmlFor="acc-name">显示名（可选）</label>
              <input
                id="acc-name"
                placeholder="用于侧栏展示"
                value={accForm.displayName}
                onChange={(e) => setAccForm((f) => ({ ...f, displayName: e.target.value }))}
              />
            </div>
            <div className="field">
              <label htmlFor="acc-host">服务器（可选）</label>
              <input
                id="acc-host"
                placeholder="如 imap.139.com:993"
                value={accForm.serverHost}
                onChange={(e) => setAccForm((f) => ({ ...f, serverHost: e.target.value }))}
              />
            </div>
          </div>
          <div className="account-form-actions">
            <button onClick={() => setAddingAccount(false)} className="btn btn--ghost">
              取消
            </button>
            <button onClick={addAccount} disabled={accountBusy} className="btn btn--primary">
              {accountBusy ? '提交中…' : '创建账户'}
            </button>
          </div>
        </section>
      )}

      {error && (
        <div className="alert error" role="alert">
          <Icon name="warning" size={16} />
          <span>{error}</span>
        </div>
      )}

      {/* 实时通知 toast 区（aria-live 供屏幕阅读器播报） */}
      <div className="toast-zone" role="status" aria-live="polite" aria-atomic="false">
        {toasts.map((t, i) => (
          <div key={`${t.ts}-${i}`} className={`toast toast-${t.kind}`}>
            <strong>
              <Icon name={NOTIF_META[t.kind]?.icon ?? 'warning'} size={14} />
              {NOTIF_META[t.kind]?.label ?? t.kind}
            </strong>
            <span>{t.preview || t.accountId}</span>
          </div>
        ))}
      </div>

          <main className="content" id="main-content">
            <section className="mail-pane" aria-label={hits ? '检索结果' : '收件箱'}>
              <div className="panel-head">
                <h2>{hits ? '检索结果' : '收件箱'}</h2>
                <span className="panel-sub">
                  {hits ? `${hits.length} 条命中` : `${mails.length} 封邮件`}
                </span>
              </div>
              {hits ? (
                <MailList hits={hits} onSelectHit={openHit} />
              ) : (
                <MailList mails={mails} loading={loading} onSelectMail={openMail} />
              )}
            </section>
            <aside className="ai-pane" aria-label="辅助面板">
              <NotificationFeed items={notifications} />
              <AiChat accountId={accountId} />
            </aside>
          </main>
        </div>
      </div>

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
