#!/usr/bin/env node
/**
 * ============================================================================
 *  移动端原生构建（build）
 * ============================================================================
 *
 *  流程：版本门禁 → 同步前端产物到 www → `cap sync <platform>` 把 www 与
 *        Capacitor 运行时插件注入原生工程 → 调用原生工具链出包。
 *
 *  用法：
 *    node scripts/build.mjs android             Android Release（APK + AAB）
 *    node scripts/build.mjs android --debug      Android Debug APK
 *    node scripts/build.mjs android --apk-only   只出 APK（不上 AAB）
 *    node scripts/build.mjs ios                 iOS Release 归档（需 macOS + Xcode）
 *    node scripts/build.mjs ios --unsigned       同上，跳过签名（CI 用；产未签名 xcarchive）
 *
 *  ──────────────────────────────────────────────────────────────────────────
 *  环境前置（本机不满足时本脚本会明确报错，不会静默产出一个空包）：
 *    Android：**JDK 21+**（Capacitor 8 的 capacitor-android 把编译级别定为
 *             Java 21，JDK 17 会报 `invalid source release: 21`）、
 *             Android SDK（ANDROID_HOME / ANDROID_SDK_ROOT）、
 *             首次还需 `sdkmanager "platforms;android-36" "build-tools;36.0.0"`
 *    iOS：    macOS + Xcode 15+（工程形态自动适配 SPM / CocoaPods）；Windows 上无法出包
 *  ──────────────────────────────────────────────────────────────────────────
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

import { prepare } from './prepare.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const PKG_DIR = path.resolve(HERE, '..')
const requireCjs = createRequire(import.meta.url)

const argv = process.argv.slice(2)
const platform = argv.find((a) => !a.startsWith('--'))
const debug = argv.includes('--debug')
const apkOnly = argv.includes('--apk-only')
const unsigned = argv.includes('--unsigned')

function resolveCapCli() {
  let pkgJson
  try {
    pkgJson = requireCjs.resolve('@capacitor/cli/package.json')
  } catch {
    throw new Error('未找到 @capacitor/cli。请先在 platforms/mobile 下执行：npm install')
  }
  const pkg = requireCjs(pkgJson)
  const bin = typeof pkg.bin === 'string' ? pkg.bin : pkg.bin?.cap
  if (!bin) throw new Error('@capacitor/cli 未声明 bin 入口')
  return path.join(path.dirname(pkgJson), bin)
}

/**
 * 读取 JDK 主版本号；取不到返回 null。
 *
 * 先读 `$JAVA_HOME/release`（JDK 9+ 都有这个文件，`JAVA_VERSION="21.0.5"`），
 * 这是最可靠的 —— 不依赖 PATH，也不会被 `java` 是别的版本骗到。
 * 读不到再退回执行 `java -version` 解析（stderr）。
 */
function javaMajorVersion() {
  const jh = process.env.JAVA_HOME
  if (jh) {
    const release = path.join(jh, 'release')
    if (fs.existsSync(release)) {
      const m = fs.readFileSync(release, 'utf8').match(/JAVA_VERSION="(\d+)/)
      if (m) return Number(m[1])
    }
  }
  const r = spawnSync('java', ['-version'], { encoding: 'utf8' })
  const m = `${r.stdout ?? ''}${r.stderr ?? ''}`.match(/version "(\d+)/)
  return m ? Number(m[1]) : null
}

const run = (cmd, args, opts = {}) => {
  const r = spawnSync(cmd, args, { stdio: 'inherit', cwd: opts.cwd ?? PKG_DIR, env: { ...process.env, ...opts.env } })
  if (r.error) throw new Error(`无法执行 ${cmd}：${r.error.message}`)
  if (r.status !== 0) throw new Error(`${cmd} ${args.join(' ')} 退出码 ${r.status}`)
  return r
}

function buildAndroid() {
  const androidDir = path.join(PKG_DIR, 'android')
  if (!fs.existsSync(androidDir)) {
    throw new Error('尚未生成 Android 工程。请先执行：npm run add:android')
  }
  if (!process.env.ANDROID_HOME && !process.env.ANDROID_SDK_ROOT) {
    throw new Error(
      '未检测到 Android SDK（ANDROID_HOME / ANDROID_SDK_ROOT 均未设置）。\n' +
        '  Android 出包需要：JDK 21+ 与 Android SDK（含 platforms;android-36、build-tools;36.0.0）。',
    )
  }

  //  JDK 主版本预检。Capacitor 8 的 android 库把编译级别定死在 Java 21
  //  （node_modules/@capacitor/android/capacitor/build.gradle:66-67 →
  //   JavaVersion.VERSION_21）。用 JDK 17 编译时 Gradle 只会给出一句
  //     Execution failed for task ':capacitor-android:compileDebugJavaWithJavac'
  //     > Java compilation initialization error
  //         error: invalid source release: 21
  //  既没有版本上下文、也不提示"去升级 JDK"，排查成本很高（实测 run 35120977342）。
  //  这里提前拦住，把"缺什么、怎么补"直接写进错误里。
  const javaMajor = javaMajorVersion()
  if (javaMajor !== null && javaMajor < 21) {
    throw new Error(
      `Android 出包需要 JDK 21+，当前 JDK 主版本为 ${javaMajor}。\n` +
        '  原因：Capacitor 8 的 capacitor-android 模块要求 sourceCompatibility = Java 21。\n' +
        '  修正：安装 Temurin 21 并把 JAVA_HOME 指向它；\n' +
        '       CI（.github/workflows/release-multiplatform.yml 的 android job）' +
        '对应 actions/setup-java 的 java-version: \'21\'。',
    )
  }
  if (javaMajor === null) {
    console.log('[mobile] 警告：无法判定 JDK 主版本（未读到 $JAVA_HOME/release，且 java 不可执行）；跳过预检')
  } else {
    console.log(`[mobile] JDK 主版本 = ${javaMajor}（Capacitor 8 要求 ≥ 21）`)
  }

  run(process.execPath, [resolveCapCli(), 'sync', 'android'])

  const isWin = process.platform === 'win32'
  const wrapper = path.join(androidDir, isWin ? 'gradlew.bat' : 'gradlew')
  if (!fs.existsSync(wrapper)) throw new Error(`未找到 Gradle wrapper：${wrapper}`)

  const tasks = debug ? ['assembleDebug'] : apkOnly ? ['assembleRelease'] : ['assembleRelease', 'bundleRelease']
  console.log(`[mobile] gradle ${tasks.join(' ')}`)
  run(wrapper, tasks, { cwd: androidDir })

  const outputs = []
  const apkDir = path.join(androidDir, 'app', 'build', 'outputs', 'apk')
  const aabDir = path.join(androidDir, 'app', 'build', 'outputs', 'bundle')
  for (const dir of [apkDir, aabDir]) {
    if (!fs.existsSync(dir)) continue
    const walk = (d) => {
      for (const n of fs.readdirSync(d)) {
        const p = path.join(d, n)
        if (fs.statSync(p).isDirectory()) walk(p)
        else if (/\.(apk|aab)$/i.test(n)) outputs.push(p)
      }
    }
    walk(dir)
  }
  return outputs
}

function buildIOS() {
  if (process.platform !== 'darwin') {
    throw new Error('iOS 出包必须在 macOS 上执行（需要 Xcode 与 CocoaPods）。当前宿主：' + process.platform)
  }
  const iosDir = path.join(PKG_DIR, 'ios')
  if (!fs.existsSync(iosDir)) throw new Error('尚未生成 iOS 工程。请先执行：npm run add:ios')

  run(process.execPath, [resolveCapCli(), 'sync', 'ios'])

  // ---------------------------------------------------------------------------
  //  工程形态自适应（Capacitor 8 起默认走 SPM，两者参数不同）
  //  ---------------------------------------------------------------------------
  //  @capacitor/cli 的 config.js 里 iosPlatformTemplateArchive 默认值是
  //  'ios-spm-template.tar.gz'；只有 `cap add ios --packagemanager CocoaPods`
  //  才会切成 'ios-pods-template.tar.gz'。实测两个模板的顶层结构：
  //    SPM  → App/App.xcodeproj + App/CapApp-SPM/Package.swift   （无顶层 xcworkspace）
  //    Pods → App/App.xcodeproj + App/App.xcworkspace + App/Podfile
  //  因此不能硬编码 -workspace：把 .xcodeproj 传给 -workspace 时 xcodebuild 会
  //  直接报 "<path> is not a workspace"，而指向不存在的 App.xcworkspace 则是
  //  "does not exist"。按实际存在的形态选参数才是稳的。
  const appDir = path.join(iosDir, 'App')
  const entries = fs.readdirSync(appDir)
  const wsName = entries.find((n) => n.endsWith('.xcworkspace'))
  const projName = entries.find((n) => n.endsWith('.xcodeproj'))
  const targetArgs = wsName
    ? ['-workspace', path.join(appDir, wsName)]
    : projName
      ? ['-project', path.join(appDir, projName)]
      : null
  if (!targetArgs) {
    throw new Error(
      `未找到 Xcode 工程/工作区（${appDir} 下既无 .xcworkspace 也无 .xcodeproj）。\n` +
        '  请先执行：npm run add:ios',
    )
  }
  console.log(`[mobile] 工程形态：${wsName ? 'CocoaPods 工作区' : 'SPM 工程'}（${wsName ?? projName}）`)

  // ---------------------------------------------------------------------------
  //  未签名归档：签名开关必须作为**命令行 build setting** 传给 xcodebuild
  //  ---------------------------------------------------------------------------
  //  只设环境变量不够：实测（Xcode 26.6 / run 35118604475）在 step env 里给了
  //  CODE_SIGNING_ALLOWED=NO 仍然失败 ——
  //    error: Signing for "App" requires a development team. Select a development
  //           team in the Signing & Capabilities editor. (in target 'App' ...)
  //  该错误在 GatherProvisioningInputs 阶段抛出，早于签名动作。作为 build setting
  //  显式传入才会真正跳过签名与 entitlements 解析。
  const signArgs = unsigned
    ? [
        'CODE_SIGNING_ALLOWED=NO',
        'CODE_SIGNING_REQUIRED=NO',
        'CODE_SIGN_IDENTITY=',
        'CODE_SIGN_ENTITLEMENTS=',
      ]
    : []

  const args = [
    ...targetArgs,
    '-scheme',
    'App',
    '-configuration',
    debug ? 'Debug' : 'Release',
    '-archivePath',
    path.join(iosDir, 'build', 'App.xcarchive'),
    ...signArgs,
    'archive',
  ]
  if (unsigned) console.log('[mobile] --unsigned：已下发 CODE_SIGNING_ALLOWED=NO 等 build setting')
  console.log('[mobile] xcodebuild archive')
  run('xcodebuild', args)
  return [path.join(iosDir, 'build', 'App.xcarchive')]
}

function main() {
  if (!['android', 'ios'].includes(platform ?? '')) {
    console.error('用法：node scripts/build.mjs <android|ios> [--debug] [--apk-only]')
    process.exit(2)
  }

  const { version } = prepare()
  console.log(`[mobile] 构建 ${platform} —— ${version.canonical}\n`)

  const outputs = platform === 'android' ? buildAndroid() : buildIOS()

  console.log(`\n[mobile] 完成。产物：`)
  for (const p of outputs) console.log(`  ${path.relative(REPO_ROOT(), p)}  ${(fs.statSync(p).size / 1024 / 1024).toFixed(1)} MB`)

  // 版本可追踪性证据：产物名或内容中应能找到规范版本号
  const needle = Buffer.from(version.canonical, 'utf8')
  for (const p of outputs) {
    if (!fs.existsSync(p) || fs.statSync(p).isDirectory()) continue
    const buf = fs.readFileSync(p)
    const hit = buf.includes(needle)
    console.log(`  ${hit ? 'OK  ' : 'WARN'}  ${path.basename(p)} 内检出版本号 ${version.canonical} = ${hit}`)
  }
}

const REPO_ROOT = () => path.resolve(PKG_DIR, '..', '..')

try {
  main()
} catch (e) {
  console.error(`\n[mobile] 失败：${e.message}`)
  process.exit(1)
}
