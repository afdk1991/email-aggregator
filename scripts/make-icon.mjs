#!/usr/bin/env node
/**
 * ============================================================================
 *  应用图标生成器（零依赖，桌面端 + HarmonyOS 共用）
 * ============================================================================
 *
 *  产出 PNG（可选同时产出多尺寸 ICO），供各平台打包使用：
 *    · Electron（Windows/macOS/Linux）→ build/icon.png + build/icon.ico
 *    · HarmonyOS                       → AppScope/.../media/app_icon.png 等
 *
 *  为什么要手写 PNG 编码器：
 *    仓库不引入任何构建期图形依赖（不使用 sharp / canvas / ImageMagick），
 *    以保证在任意 CI runner 上 `npm ci && npm run dist` 都能直接从源码产出图标。
 *    PNG 的最小可用实现只需 IHDR + IDAT(zlib-deflate) + IEND 三个块，加上 CRC32
 *    即可，约 60 行 —— 远比引入原生依赖划算，也避免了二进制素材入库。
 *
 *  设计：与前端设计系统同源的主色渐变 #3b82f6 → #1d4ed8，
 *        圆角方形容器 + 白色信封（index.html 里 📬 favicon 的几何化）。
 *        4× 超采样抗锯齿，避免小尺寸下边缘发毛。
 *
 *  用法：
 *    node scripts/make-icon.mjs                          默认 → platforms/desktop/build
 *    node scripts/make-icon.mjs --out <dir>              指定输出目录
 *    node scripts/make-icon.mjs --png app_icon.png       指定 PNG 文件名
 *    node scripts/make-icon.mjs --size 512               指定 PNG 边长
 *    node scripts/make-icon.mjs --no-ico                 不产出 ICO
 * ============================================================================
 */

import fs from 'node:fs'
import path from 'node:path'
import zlib from 'node:zlib'
import { fileURLToPath, pathToFileURL } from 'node:url'

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// ============================================================================
//  PNG 编码
// ============================================================================

const CRC_TABLE = (() => {
  const t = new Int32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    t[n] = c
  }
  return t
})()

function crc32(buf) {
  let c = 0xffffffff
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8)
  return (c ^ 0xffffffff) >>> 0
}

function chunk(type, data) {
  const len = Buffer.alloc(4)
  len.writeUInt32BE(data.length, 0)
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data])
  const crc = Buffer.alloc(4)
  crc.writeUInt32BE(crc32(body), 0)
  return Buffer.concat([len, body, crc])
}

/** @param {Uint8Array} rgba 长度 = w*h*4 的 RGBA 像素 */
export function encodePng(rgba, w, h) {
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(w, 0)
  ihdr.writeUInt32BE(h, 4)
  ihdr[8] = 8 // bit depth
  ihdr[9] = 6 // color type: RGBA
  ihdr[10] = 0
  ihdr[11] = 0
  ihdr[12] = 0

  // 每行前置一个 filter 字节（0 = None）。图标是平滑渐变，用 None 已足够，
  // 依赖 zlib 自身的 DEFLATE 即可把体积压到几十 KB。
  const stride = w * 4
  const raw = Buffer.alloc((stride + 1) * h)
  for (let y = 0; y < h; y++) {
    raw[y * (stride + 1)] = 0
    Buffer.from(rgba.buffer, rgba.byteOffset + y * stride, stride).copy(raw, y * (stride + 1) + 1)
  }

  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ])
}

// ============================================================================
//  几何与着色
// ============================================================================

const clamp01 = (v) => (v < 0 ? 0 : v > 1 ? 1 : v)

/** 点 (x,y) 是否落在圆角矩形内（坐标均为 0..1 归一化） */
function inRoundedRect(x, y, x0, y0, x1, y1, r) {
  if (x < x0 || x > x1 || y < y0 || y > y1) return false
  const cx = x < x0 + r ? x0 + r : x > x1 - r ? x1 - r : x
  const cy = y < y0 + r ? y0 + r : y > y1 - r ? y1 - r : y
  if (cx === x && cy === y) return true
  const dx = x - cx
  const dy = y - cy
  return dx * dx + dy * dy <= r * r
}

function distToSegment(px, py, ax, ay, bx, by) {
  const vx = bx - ax
  const vy = by - ay
  const len2 = vx * vx + vy * vy
  const t = len2 === 0 ? 0 : clamp01(((px - ax) * vx + (py - ay) * vy) / len2)
  const dx = px - (ax + t * vx)
  const dy = py - (ay + t * vy)
  return Math.sqrt(dx * dx + dy * dy)
}

/** 主色渐变端点（与前端设计系统 --brand 系同源） */
const TOP = { r: 0x3b, g: 0x82, b: 0xf6 }
const BOTTOM = { r: 0x1d, g: 0x4e, b: 0xd8 }

/** 对单点采样，返回 [r,g,b,a]（0..255）。 */
function sample(x, y) {
  // 圆角方形卡片（留 4% 安全边距，符合各平台图标规范）
  if (!inRoundedRect(x, y, 0.04, 0.04, 0.96, 0.96, 0.21)) return [0, 0, 0, 0]

  const t = clamp01((y - 0.04) / 0.92)
  const r = Math.round(TOP.r + (BOTTOM.r - TOP.r) * t)
  const g = Math.round(TOP.g + (BOTTOM.g - TOP.g) * t)
  const b = Math.round(TOP.b + (BOTTOM.b - TOP.b) * t)

  // 信封折角：两段线段构成的 "V"，刻意保持卡片底色（而非白色），
  // 在白色信封上呈现为主色描边，轮廓比整块纯白更清晰可辨。
  const FLAP_W = 0.026
  const flap =
    distToSegment(x, y, 0.255, 0.375, 0.5, 0.535) <= FLAP_W ||
    distToSegment(x, y, 0.5, 0.535, 0.745, 0.375) <= FLAP_W

  if (inRoundedRect(x, y, 0.24, 0.36, 0.76, 0.665, 0.035) && !flap) return [255, 255, 255, 255]
  return [r, g, b, 255]
}

/** 超采样渲染。 */
export function render(size, ss = 4) {
  const out = new Uint8Array(size * size * 4)
  const inv = 1 / (size * ss)
  for (let py = 0; py < size; py++) {
    for (let px = 0; px < size; px++) {
      let r = 0
      let g = 0
      let b = 0
      let a = 0
      for (let sy = 0; sy < ss; sy++) {
        for (let sx = 0; sx < ss; sx++) {
          const c = sample((px * ss + sx + 0.5) * inv, (py * ss + sy + 0.5) * inv)
          const ca = c[3] / 255
          r += c[0] * ca
          g += c[1] * ca
          b += c[2] * ca
          a += c[3]
        }
      }
      const n = ss * ss
      const avgA = a / n
      const i = (py * size + px) * 4
      if (avgA > 0) {
        // 反预乘：累加时用了预乘 alpha，还原为直通 alpha 才能被解码器正确合成
        const norm = a / 255
        out[i] = Math.round(r / norm)
        out[i + 1] = Math.round(g / norm)
        out[i + 2] = Math.round(b / norm)
      }
      out[i + 3] = Math.round(avgA)
    }
  }
  return out
}

// ============================================================================
//  ICO 容器（Vista+ 允许每个条目直接内嵌 PNG）
// ============================================================================

function buildIco(entries) {
  const header = Buffer.alloc(6)
  header.writeUInt16LE(0, 0) // reserved
  header.writeUInt16LE(1, 2) // type: 1 = icon
  header.writeUInt16LE(entries.length, 4)

  const dir = Buffer.alloc(16 * entries.length)
  let offset = 6 + 16 * entries.length
  entries.forEach((e, i) => {
    const o = i * 16
    dir[o] = e.size >= 256 ? 0 : e.size // 0 表示 256
    dir[o + 1] = e.size >= 256 ? 0 : e.size
    dir[o + 2] = 0 // palette
    dir[o + 3] = 0 // reserved
    dir.writeUInt16LE(1, o + 4) // color planes
    dir.writeUInt16LE(32, o + 6) // bits per pixel
    dir.writeUInt32LE(e.png.length, o + 8)
    dir.writeUInt32LE(offset, o + 12)
    offset += e.png.length
  })

  return Buffer.concat([header, dir, ...entries.map((e) => e.png)])
}

// ============================================================================
//  CLI
// ============================================================================

const argv = process.argv.slice(2)
const getOpt = (name, dflt) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : dflt)

function main() {
  const outDir = path.resolve(REPO, getOpt('--out', 'platforms/desktop/build'))
  const pngName = getOpt('--png', 'icon.png')
  const size = Number(getOpt('--size', '1024'))
  const wantIco = !argv.includes('--no-ico')

  fs.mkdirSync(outDir, { recursive: true })

  const png = encodePng(render(size), size, size)
  const pngPath = path.join(outDir, pngName)
  fs.writeFileSync(pngPath, png)

  const kb = (n) => `${(n / 1024).toFixed(1)} KB`
  console.log(`[icon] ${path.relative(REPO, pngPath)}  ${size}×${size}  ${kb(png.length)}`)

  if (wantIco) {
    const icoSizes = [16, 32, 48, 64, 128, 256]
    const ico = buildIco(icoSizes.map((s) => ({ size: s, png: encodePng(render(s, 4), s, s) })))
    const icoPath = path.join(outDir, 'icon.ico')
    fs.writeFileSync(icoPath, ico)
    console.log(`[icon] ${path.relative(REPO, icoPath)}  ${icoSizes.join('/')}  ${kb(ico.length)}`)
  }
}

const invokedDirectly =
  process.argv[1] !== undefined && pathToFileURL(process.argv[1]).href === import.meta.url

if (invokedDirectly) main()
