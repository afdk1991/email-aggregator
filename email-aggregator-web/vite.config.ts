import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 前端开发服务器：将 /api 与 /ws 反向代理到本地 Go 主干版后端。
// 这样前端无需关心跨域，直接以同源路径调用后端 REST/WS 契约。
//
// 端口说明（本机三项目并存）：Go 后端以 HTTP_PORT=8090 启动，
// 因为 8080 由项目002 的 gateway-go 固定占用。可用 VITE_API_PORT 覆盖。
const apiPort = process.env.VITE_API_PORT ?? '8090'

export default defineConfig({
  plugins: [react()],
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
})
