import { Component, type ReactNode } from 'react'
import { Icon } from './Icon'

interface Props {
  children: ReactNode
}

interface State {
  hasError: boolean
  message: string
}

// 全局错误边界：捕获渲染期抛错，避免单一组件异常导致整页白屏。
// 提供可恢复 UI（错误卡片 + 重试），而非崩溃堆栈。
export default class ErrorBoundary extends Component<Props, State> {
  state: State = { hasError: false, message: '' }

  static getDerivedStateFromError(err: unknown): State {
    const message = err instanceof Error ? err.message : String(err)
    return { hasError: true, message }
  }

  componentDidCatch(err: unknown, info: unknown) {
    // 生产环境可在此上报到监控；本 PoC 仅控制台留痕。
    console.error('[ErrorBoundary]', err, info)
  }

  handleRetry = () => {
    this.setState({ hasError: false, message: '' })
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="error-boundary" role="alert">
          <h2>
            <Icon name="warning" size={20} /> 页面出错
          </h2>
          <p className="error-boundary-msg">{this.state.message || '发生未知错误'}</p>
          <button onClick={this.handleRetry} className="btn btn--primary btn--lg">
            重新加载
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
