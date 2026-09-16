'use strict'
/**
 * 桌面端预加载脚本 —— 向页面注入**最小**的桌面能力。
 *
 * 安全约定：只暴露只读的元信息，不暴露 ipcRenderer 本体。
 * 页面（SPA）在任何平台上都能运行，桌面元信息是纯增强：
 * 缺失时前端必须能正常降级，因此这里不做任何强制依赖。
 */

const { contextBridge } = require('electron')
const APP_VERSION = require('./version.generated.cjs')

contextBridge.exposeInMainWorld('eaDesktop', {
  /** 运行平台：'win32' | 'darwin' | 'linux' */
  platform: process.platform,
  /** CPU 架构：'x64' | 'arm64' | ... */
  arch: process.arch,
  /** 与 version.json 同源的三形态版本号 */
  version: {
    canonical: APP_VERSION.canonical,
    semver: APP_VERSION.semver,
    code: APP_VERSION.code,
  },
  /** 运行形态标识，供前端区分「浏览器 / 桌面 / 移动 / 鸿蒙」 */
  runtime: 'electron',
  electron: process.versions.electron,
})
