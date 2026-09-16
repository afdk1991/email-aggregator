import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

// 前端开发服务器：将 /api 与 /ws 反向代理到本地 Go 主干版后端。
// 这样前端无需关心跨域，直接以同源路径调用后端 REST/WS 契约。
//
// 端口说明（本机三项目并存）：Go 后端以 HTTP_PORT=8090 启动，
// 因为 8080 由项目002 的 gateway-go 固定占用。可用 VITE_API_PORT 覆盖。
//
// 演示模式开关：从 .env / .env.production 读 VITE_DEMO_MODE，
// 通过 define 做构建期常量替换（而非运行期判断），
// 关闭后相关分支会被 minifier 折叠、死代码被 tree-shaking 掉，
// 生产产物里不会残留演示文案与演示接口调用。
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const apiPort = env.VITE_API_PORT || '8090'
  const demoMode = env.VITE_DEMO_MODE !== 'false'

  return {
    plugins: [react()],
    define: {
      __DEMO_MODE__: JSON.stringify(demoMode),
    },
    server: {
      port: 5173,
      strictPort: true,
      proxy: {
        '/api': {
          target: `http://localhost:${apiPort}`,
          changeOrigin: true,
        },
        '/ws': {
          target: `ws://localhost:${apiPort}`,
          ws: true,
        },
      },
    },
  }
})
