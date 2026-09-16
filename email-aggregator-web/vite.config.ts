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
//
// base 说明（多端部署的关键）：
//   生产构建统一用**相对路径** base='./'，让同一份 dist 能落到五处目标：
//     · Web 直访 / EdgeOne 静态站点 —— 页面在根路径，'./assets/x.js' 等价于 '/assets/x.js'
//     · Go 单二进制（webui 标签）    —— 挂在根路径，同上
//     · Electron 桌面端              —— 加载 127.0.0.1:<port>/index.html，相对路径成立
//     · Capacitor 移动端             —— origin 为 https://localhost，相对路径成立
//     · HarmonyOS Web 容器           —— 以 $rawfile('www/index.html') 加载。此处若用
//                                      绝对路径 '/assets/x.js'，会解析到 rawfile 根目录
//                                      而漏掉 www/ 一层，资源全部 404 —— 这是多端
//                                       场景下最容易踩、又最难定位的一个坑。
//   开发服务器仍用 '/'，避免 Vite dev 的 HMR 路径解析异常。
export default defineConfig(({ mode, command }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const apiPort = env.VITE_API_PORT || '8090'
  const demoMode = env.VITE_DEMO_MODE !== 'false'
  const isServe = command === 'serve'

  return {
    base: isServe ? '/' : './',
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
