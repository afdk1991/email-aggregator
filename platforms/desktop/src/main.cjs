'use strict'
/**
 * ============================================================================
 *  邮箱聚合平台 · 桌面端主进程（Electron）
 *
 *  职责：把「Go 主干 + React SPA」这一套 Web 形态，包装为 Windows / macOS /
 *        Linux 上可双击运行的原生桌面应用。
 *
 *  启动流程（两条路，优先走第一条）：
 *    ① sidecar 路径：以随机空闲端口拉起随包分发的 Go 单二进制
 *       （`go build -tags webui ./cmd/server`，REST + WebSocket + SPA 同端口），
 *       健康检查通过后，窗口直接加载 http://127.0.0.1:<port>。
 *       优点：只有一个 origin，无跨域、无代理层，前后端契约与线上完全一致。
 *    ② 静态兜底：sidecar 缺失或启动失败（例如杀软拦截未签名 exe、架构不匹配）时，
 *       在回环地址上起一个只读静态服务器托管内嵌 SPA。
 *       此时页面正常渲染，/api/* 返回 503，UI 的健康徽标会显示「后端离线」——
 *       即「优雅降级」而非白屏。
 *
 *  设计取舍：
 *    · 主进程用纯 CommonJS（.cjs），不引入 TS 构建步骤。理由：主进程只有窗口与进程
 *      生命周期逻辑，没有业务类型需要约束；而引入 tsc 步骤会让「打包」依赖「先构建」，
 *      与 electron-builder 的 files 直接打包 src/ 相冲突。
 *    · sandbox 关闭但 contextIsolation 保持开启。sandbox 下 preload 无法 require
 *      相对路径模块（版本号常量），且 nodeIntegration 仍为 false，安全性由
 *      contextIsolation + 最小 IPC 暴露面保证。
 * ============================================================================
 */

const { app, BrowserWindow, Menu, shell, dialog } = require('electron')
const path = require('node:path')
const fs = require('node:fs')
const http = require('node:http')
const net = require('node:net')
const { spawn } = require('node:child_process')

const APP_VERSION = require('./version.generated.cjs')

const isDev = !app.isPackaged
/** 打包后 extraResources 落在 process.resourcesPath；开发态用仓库内的 resources/。 */
const RESOURCES = isDev ? path.join(__dirname, '..', 'resources') : process.resourcesPath
const LOG_PREFIX = '[ea-desktop]'

let mainWindow = null
let backend = null // { proc, url, port }
let staticServer = null // { server, url }

const log = (...a) => console.log(LOG_PREFIX, ...a)

// ============================================================================
//  Go 主干 sidecar 生命周期
// ============================================================================

/** 按当前平台/架构推导 sidecar 文件名，与 email-aggregator-go/build.ps1 的命名规则一致。 */
function sidecarName() {
  const osName = { win32: 'windows', darwin: 'darwin', linux: 'linux' }[process.platform]
  if (!osName) return null
  const archName = process.arch === 'arm64' ? 'arm64' : 'amd64'
  const ext = process.platform === 'win32' ? '.exe' : ''
  return `email-aggregator-${osName}-${archName}${ext}`
}

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.unref()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address()
      srv.close(() => resolve(port))
    })
  })
}

/** 轮询 /api/health，直到 200 或超时。 */
function waitHealthy(base, timeoutMs) {
  const deadline = Date.now() + timeoutMs
  return new Promise((resolve) => {
    const retry = () => {
      if (Date.now() > deadline) return resolve(false)
      setTimeout(tick, 300)
    }
    const tick = () => {
      const req = http.get(`${base}/api/health`, (res) => {
        res.resume()
        if (res.statusCode === 200) return resolve(true)
        retry()
      })
      req.once('error', retry)
      req.setTimeout(1500, () => req.destroy())
    }
    tick()
  })
}

async function startBackend() {
  const name = sidecarName()
  if (!name) {
    log(`未支持的平台 ${process.platform}/${process.arch}，跳过 sidecar`)
    return null
  }
  const bin = path.join(RESOURCES, 'bin', name)
  if (!fs.existsSync(bin)) {
    log(`sidecar 不存在：${bin} —— 改用静态兜底`)
    return null
  }

  const port = await freePort()
  const url = `http://127.0.0.1:${port}`

  let proc
  try {
    proc = spawn(bin, [], {
      env: {
        ...process.env,
        // 只认 HTTP_PORT（Go 主干不读 PORT），由本进程分配空闲端口避免与
        // 用户机上其它服务（如同为 8080 的项目）冲突。
        HTTP_PORT: String(port),
        // 桌面端为本地单用户形态：允许演示端点，首次启动即有可浏览的种子数据。
        ENABLE_DEMO: 'true',
        EA_DESKTOP: '1',
        EA_APP_VERSION: APP_VERSION.canonical,
      },
      cwd: path.dirname(bin),
      stdio: ['ignore', 'pipe', 'pipe'],
      windowsHide: true,
    })
  } catch (e) {
    log('sidecar 启动异常：', e.message)
    return null
  }

  proc.stdout.on('data', (d) => process.stdout.write(`${LOG_PREFIX} [go] ${d}`))
  proc.stderr.on('data', (d) => process.stderr.write(`${LOG_PREFIX} [go:err] ${d}`))
  proc.once('exit', (code, signal) => {
    log(`sidecar 退出 code=${code} signal=${signal}`)
    backend = null
  })

  const ok = await waitHealthy(url, 15000)
  if (!ok) {
    log('sidecar 健康检查超时，终止并改用静态兜底')
    try {
      proc.kill()
    } catch {
      /* 忽略 */
    }
    return null
  }

  log(`sidecar 就绪：${url}（${APP_VERSION.canonical}）`)
  backend = { proc, url, port }
  return backend
}

function stopBackend() {
  if (!backend?.proc) return
  const { proc } = backend
  backend = null
  try {
    if (process.platform === 'win32') {
      // Windows 上 Go 进程无子进程，直接 kill 即可；用 taskkill 兜底确保句柄释放
      spawn('taskkill', ['/pid', String(proc.pid), '/f', '/t'], { windowsHide: true })
    } else {
      proc.kill('SIGTERM')
    }
  } catch (e) {
    log('结束 sidecar 失败：', e.message)
  }
}

// ============================================================================
//  静态兜底服务器（托管内嵌 SPA）
// ============================================================================

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.webp': 'image/webp',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
  '.map': 'application/json; charset=utf-8',
}

/**
 * 起一个只读静态服务器。
 * 关键点：/api/* 与 /ws 一律返回 503 JSON，**绝不回退 index.html** ——
 * 否则「后端不可用」会被伪装成「200 + HTML」，前端 JSON.parse 抛错而看不出根因。
 * 这一约束与 Go 侧 src/api/static_webui.go 的 API 命名空间处理保持一致。
 */
function startStaticServer(rootDir) {
  if (!fs.existsSync(rootDir)) {
    log(`静态兜底目录不存在：${rootDir}`)
    return Promise.resolve(null)
  }
  return new Promise((resolve, reject) => {
    const server = http.createServer((req, res) => {
      const pathname = decodeURIComponent(new URL(req.url, 'http://127.0.0.1').pathname)

      if (pathname === '/api' || pathname.startsWith('/api/') || pathname === '/ws') {
        res.writeHead(503, { 'content-type': 'application/json; charset=utf-8' })
        res.end(
          JSON.stringify({
            error: '本地服务未就绪',
            detail: '随包分发的内置服务未能启动，页面已降级为只读模式。请重新启动应用或查看日志。',
            version: APP_VERSION.canonical,
          }),
        )
        return
      }

      const rel = pathname === '/' ? 'index.html' : pathname.replace(/^\/+/, '')
      const abs = path.join(rootDir, rel)
      // 目录穿越防护：解析后必须仍位于 rootDir 之内
      if (!abs.startsWith(rootDir + path.sep) && abs !== path.join(rootDir, 'index.html')) {
        res.writeHead(403).end('forbidden')
        return
      }

      fs.readFile(abs, (err, buf) => {
        if (err) {
          // SPA 无客户端路由，未命中的资源一律回首页；无 index.html 则 404
          const index = path.join(rootDir, 'index.html')
          return fs.readFile(index, (e2, html) => {
            if (e2) return res.writeHead(404).end('not found')
            res.writeHead(200, { 'content-type': MIME['.html'] }).end(html)
          })
        }
        res.writeHead(200, { 'content-type': MIME[path.extname(abs)] ?? 'application/octet-stream' })
        res.end(buf)
      })
    })

    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address()
      staticServer = { server, url: `http://127.0.0.1:${port}` }
      log(`静态兜底就绪：${staticServer.url}（root=${rootDir}）`)
      resolve(staticServer)
    })
  })
}

// ============================================================================
//  窗口
// ============================================================================

function buildMenu() {
  const isMac = process.platform === 'darwin'
  const template = []

  if (isMac) {
    template.push({
      label: app.name,
      submenu: [
        { label: `关于 邮箱聚合平台 ${APP_VERSION.canonical}`, role: 'about' },
        { type: 'separator' },
        { role: 'hide', label: '隐藏' },
        { role: 'hideOthers', label: '隐藏其他' },
        { role: 'unhide', label: '全部显示' },
        { type: 'separator' },
        { role: 'quit', label: '退出' },
      ],
    })
  }

  template.push({
    label: '文件',
    submenu: [
      { label: '重新加载界面', accelerator: 'CmdOrCtrl+R', click: () => mainWindow?.webContents.reload() },
      { type: 'separator' },
      isMac ? { role: 'close', label: '关闭窗口' } : { role: 'quit', label: '退出' },
    ],
  })

  template.push({
    label: '编辑',
    submenu: [
      { role: 'undo', label: '撤销' },
      { role: 'redo', label: '重做' },
      { type: 'separator' },
      { role: 'cut', label: '剪切' },
      { role: 'copy', label: '复制' },
      { role: 'paste', label: '粘贴' },
      { role: 'selectAll', label: '全选' },
    ],
  })

  template.push({
    label: '视图',
    submenu: [
      { role: 'resetZoom', label: '实际大小' },
      { role: 'zoomIn', label: '放大' },
      { role: 'zoomOut', label: '缩小' },
      { type: 'separator' },
      { role: 'togglefullscreen', label: '全屏' },
      { type: 'separator' },
      { role: 'toggleDevTools', label: '开发者工具' },
    ],
  })

  template.push({
    label: '帮助',
    submenu: [
      {
        label: `关于（${APP_VERSION.canonical}）`,
        click: () => {
          dialog.showMessageBox(mainWindow, {
            type: 'info',
            title: '关于 邮箱聚合平台',
            message: `邮箱聚合平台 ${APP_VERSION.canonical}`,
            detail: [
              `版本号（canonical）：${APP_VERSION.canonical}`,
              `平台合规号（semver）：${APP_VERSION.semver}`,
              `单调整数码（code）：${APP_VERSION.code}`,
              `平台：${process.platform} / ${process.arch}`,
              `Electron：${process.versions.electron}`,
              `Chromium：${process.versions.chrome}`,
              '',
              backend ? `本地服务：${backend.url}` : '本地服务：未启动（已降级为只读模式）',
            ].join('\n'),
            noLink: true,
            buttons: ['好'],
          })
        },
      },
      {
        label: '项目主页',
        click: () => shell.openExternal('https://github.com/afdk1991/email-aggregator'),
      },
    ],
  })

  Menu.setApplicationMenu(Menu.buildFromTemplate(template))
}

async function createWindow() {
  const backendInfo = await startBackend()

  let target = null
  if (backendInfo) {
    target = backendInfo.url
  } else {
    const fallback = await startStaticServer(path.join(RESOURCES, 'app-dist'))
    target = fallback?.url ?? null
  }

  mainWindow = new BrowserWindow({
    width: 1280,
    height: 840,
    minWidth: 960,
    minHeight: 620,
    show: false,
    backgroundColor: '#f6f7f9',
    title: `邮箱聚合平台 ${APP_VERSION.canonical}`,
    // 顶栏由 SPA 自行绘制，桌面端不自绘标题栏，避免与设计系统冲突
    autoHideMenuBar: process.platform !== 'darwin',
    webPreferences: {
      preload: path.join(__dirname, 'preload.cjs'),
      contextIsolation: true,
      nodeIntegration: false,
      // 见文件头「设计取舍」：sandbox 关闭以允许 preload require 版本常量模块
      sandbox: false,
      spellcheck: false,
    },
  })

  mainWindow.once('ready-to-show', () => mainWindow.show())
  mainWindow.on('closed', () => {
    mainWindow = null
  })

  // 外部链接交给系统浏览器；站内导航保持在应用窗口内
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    if (/^https?:/i.test(url)) shell.openExternal(url)
    return { action: 'deny' }
  })

  if (target) {
    await mainWindow.loadURL(target)
  } else {
    // 连 SPA 产物都没有（异常构建）：显式告知，而不是留一个白窗口
    await mainWindow.loadURL(
      'data:text/html;charset=utf-8,' +
        encodeURIComponent(
          `<body style="font:14px/1.6 system-ui;padding:40px;color:#101828">
             <h2>无法启动界面</h2>
             <p>未找到内置服务与静态页面产物（<code>resources/app-dist</code>）。</p>
             <p>版本：${APP_VERSION.canonical}</p>
           </body>`,
        ),
    )
  }

  return mainWindow
}

// ============================================================================
//  应用生命周期
// ============================================================================

if (!app.requestSingleInstanceLock()) {
  app.quit()
} else {
  app.on('second-instance', () => {
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore()
      mainWindow.focus()
    }
  })

  app.whenReady().then(async () => {
    log(`启动 ${APP_VERSION.canonical}（electron ${process.versions.electron}, ${process.platform}/${process.arch}）`)
    buildMenu()
    await createWindow()

    app.on('activate', () => {
      if (BrowserWindow.getAllWindows().length === 0) createWindow()
    })
  })

  app.on('window-all-closed', () => {
    if (process.platform !== 'darwin') app.quit()
  })

  app.on('before-quit', () => {
    stopBackend()
    staticServer?.server?.close()
  })

  app.on('will-quit', () => {
    stopBackend()
  })
}

// 未捕获异常不应静默退出——桌面端用户看不到控制台，弹窗是唯一反馈渠道
process.on('uncaughtException', (e) => {
  log('未捕获异常：', e)
  try {
    dialog.showErrorBox('邮箱聚合平台发生错误', `${APP_VERSION.canonical}\n\n${e?.stack ?? e}`)
  } catch {
    /* 忽略 */
  }
})
