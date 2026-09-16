/**
 * 邮箱聚合平台 · HarmonyOS 入口 Ability
 *
 * 职责与其他端保持一致：只负责「把统一 SPA 装进原生容器」。
 * 版本号来自 ../generated/AppVersion（由 scripts/version.mjs 从 version.json 生成），
 * 与 Web / 桌面 / 移动端完全同源。
 */

import { AbilityConstant, UIAbility, Want } from '@kit.AbilityKit'
import { hilog } from '@kit.PerformanceAnalysisKit'
import { window } from '@kit.ArkUI'

import { APP_VERSION_CANONICAL } from '../generated/AppVersion'

const DOMAIN: number = 0x0000
const TAG: string = 'EmailAggregator'

export default class EntryAbility extends UIAbility {
  onCreate(want: Want, launchParam: AbilityConstant.LaunchParam): void {
    hilog.info(DOMAIN, TAG, 'onCreate, version=%{public}s', APP_VERSION_CANONICAL)
  }

  onDestroy(): void {
    hilog.info(DOMAIN, TAG, 'onDestroy')
  }

  onWindowStageCreate(windowStage: window.WindowStage): void {
    windowStage.loadContent('pages/Index', (err) => {
      if (err.code) {
        hilog.error(DOMAIN, TAG, 'loadContent failed: %{public}s', JSON.stringify(err))
        return
      }
      hilog.info(DOMAIN, TAG, 'loadContent succeeded')
    })
  }

  onWindowStageDestroy(): void {
    hilog.info(DOMAIN, TAG, 'onWindowStageDestroy')
  }

  onForeground(): void {
    hilog.info(DOMAIN, TAG, 'onForeground')
  }

  onBackground(): void {
    hilog.info(DOMAIN, TAG, 'onBackground')
  }
}
