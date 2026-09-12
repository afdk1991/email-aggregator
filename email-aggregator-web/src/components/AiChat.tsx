import { useState } from 'react'
import { aiChat } from '../api/client'
import type { ChatMessage, ChatRequest } from '../types'
import { Icon } from './Icon'

interface Props {
  accountId: string
}

export default function AiChat({ accountId }: Props) {
  const [messages, setMessages] = useState<ChatMessage[]>([])
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const send = async () => {
    const text = input.trim()
    if (!text || busy) return
    const next: ChatMessage[] = [...messages, { role: 'user', content: text }]
    setMessages(next)
    setInput('')
    setBusy(true)
    setError(null)
    try {
      const req: ChatRequest = {
        tenantId: 'demo-tenant',
        tenantTier: 'public',
        accountId,
        capability: 'chat',
        messages: next,
        sensitivity: 1,
        userConsented: true,
      }
      const resp = await aiChat(req)
      setMessages([...next, { role: 'model', content: resp.content }])
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e)
      // 占位无真实 LLM 时后端返回 422/405；映射为明确的"未配置"提示，而非裸 HTTP 错误。
      setError(
        /422|405|404|not found|gateway|ai/i.test(msg)
          ? 'AI 网关未配置：后端未接入真实 LLM 端点（当前为占位）'
          : msg,
      )
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="ai-chat">
      <h2>AI 助手（ADR-010）</h2>
      <div className="ai-messages">
        {messages.length === 0 && (
          <p className="empty">向 AI 提问（需 integration 构建挂载 AI 网关）</p>
        )}
        {messages.map((m, i) => (
          <div key={i} className={`ai-msg ${m.role}`}>
            <b>{m.role === 'user' ? '你' : 'AI'}</b>：{m.content}
          </div>
        ))}
        {error && <div className="error">AI 错误：{error}</div>}
      </div>
      <div className="ai-input">
        <textarea
          value={input}
          aria-label="输入 AI 问题"
          placeholder="输入问题…"
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              send()
            }
          }}
        />
        <button onClick={send} disabled={busy} aria-label="发送提问">
          {busy ? '思考中…' : (
            <>
              发送 <Icon name="send" size={14} />
            </>
          )}
        </button>
      </div>
    </div>
  )
}
