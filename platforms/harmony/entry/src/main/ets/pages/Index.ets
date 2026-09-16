/**
 * ============================================================================
 *  邮箱聚合平台 · HarmonyOS 主页面（Web 容器）
 * ============================================================================
 *
 *  为什么用 ArkTS + Web 而不是纯 ArkTS 重写 UI：
 *    项目唯一的 UI 源码是 React + Vite 的 SPA。多端策略是「一份 SPA，多副外壳」，
 *    各端外壳只负责容器与原生能力。用 ArkTS 重写会让六端出现两套 UI 与两套契约，
 *    这正是本项目在 TS PoC / Go 主干上刻意避免过的问题。
 *
 *  资源加载：
 *    `$rawfile('www/index.html')` 对应 entry/src/main/resources/rawfile/www/，
 *    该目录由 scripts/sync-web-assets.mjs 从 email-aggregator-web/dist 同步。
 *    ⚠️ 此处必须搭配 vite 的 base='./'（相对路径）。若 SPA 产物用绝对路径
 *    '/assets/x.js'，在 rawfile 协议下会解析到 rawfile 根目录而漏掉 www/ 一层，
 *    表现为「白屏 + 资源 404」——多端场景里最难定位的坑之一。
 *
 *  原生桥：
 *    javaScriptProxy 以 `eaHarmony` 注入页面全局对象，暴露 getVersion 等只读方法，
 *    使页面能识别运行形态并展示版本号（与其他端一致的 MALLVx.y.z）。
 * ============================================================================
 */

import { webview } from '@kit.ArkWeb'
import { hilog } from '@kit.PerformanceAnalysisKit'

import { APP_VERSION_CANONICAL, APP_VERSION_CODE, APP_VERSION_SEMVER } from '../generated/AppVersion'

const DOMAIN: number = 0x0000
const TAG: string = 'EmailAggregator'

/** 注入页面的原生桥对象。方法名必须与 methodList 完全一致。 */
class EaHarmonyBridge {
  getVersion(): string {
    return APP_VERSION_CANONICAL
  }

  getVersionSemver(): string {
    return APP_VERSION_SEMVER
  }

  getVersionCode(): number {
    return APP_VERSION_CODE
  }

  /** 运行形态标识，供页面区分 浏览器 / 桌面 / 移动 / 鸿蒙 */
  getRuntime(): string {
    return 'harmony'
  }
}

@Entry
@Component
struct Index {
  private controller: webview.WebviewController = new webview.WebviewController()
  private bridge: EaHarmonyBridge = new EaHarmonyBridge()

  build() {
    Column() {
      Web({ src: $rawfile('www/index.html'), controller: this.controller })
        .javaScriptAccess(true)
        .domStorageAccess(true)
        .fileAccess(true)
        .imageAccess(true)
        .onlineImageAccess(true)
        // 生产形态不放开混合内容：页面与 API 均应走 https
        .mixedMode(MixedMode.None)
        // 桌面级应用不需要双指缩放，避免与列表滚动冲突
        .zoomAccess(false)
        .javaScriptProxy({
          object: this.bridge,
          name: 'eaHarmony',
          methodList: ['getVersion', 'getVersionSemver', 'getVersionCode', 'getRuntime'],
          controller: this.controller,
        })
        .onPageEnd(() => {
          hilog.info(DOMAIN, TAG, 'page loaded, version=%{public}s', APP_VERSION_CANONICAL)
        })
        .onErrorReceive((event) => {
          hilog.error(DOMAIN, TAG, 'load error: %{public}s', event.error.getErrorInfo())
        })
    }
    .width('100%')
    .height('100%')
    .backgroundColor('#f6f7f9')
  }
}
