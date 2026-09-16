/**
 * ============================================================================
 *  Info.plist 读取（XML plist + 二进制 plist）
 * ============================================================================
 *
 *  为什么需要它：
 *    `version.mjs sync` 写入的是 `ios/App/App/Info.plist`（XML 文本），但 Xcode
 *    构建时会把 Info.plist **重新序列化成二进制 plist（bplist00）** 打进 App 包。
 *    于是「校验 .xcarchive 里的版本号」如果只用文本正则去匹配
 *    `<key>CFBundleVersion</key>`，必然匹配不到 —— 得到假阴性。
 *
 *  为什么不调 `plutil`：plutil 只存在于 macOS，会让这条检查在 Windows 开发机
 *  上静默失效、无法本地复现。这里用纯 JS 实现 bplist00 的最小读取器，两端行为
 *  一致且可本地测试，不引入任何外部命令与第三方依赖。
 *
 *  只实现本场景所需的最小子集：顶层字典 + 字符串/整数/实数标量值。
 *  Info.plist 里 CFBundleShortVersionString / CFBundleVersion 都是字符串。
 * ============================================================================
 */

import fs from 'node:fs'

const BPLIST_MAGIC = 'bplist00'

/** 大端读整数。8 字节必须走 BigInt —— buf.readUIntBE 最多只支持 6 字节。 */
function readUint(buf, off, size) {
  if (size === 8) return Number(buf.readBigUInt64BE(off))
  return buf.readUIntBE(off, size)
}

/**
 * 解析二进制 plist 的顶层字典。
 *
 * @param {Buffer} buf
 * @returns {Record<string,string>|null} 顶层字典的标量键值；非 bplist00、
 *   结构异常或顶层不是字典时返回 null。
 */
export function readBinaryPlist(buf) {
  if (buf.length < 40 || buf.toString('latin1', 0, 8) !== BPLIST_MAGIC) return null
  try {
    // 尾部 32 字节 trailer：6 保留 + offsetIntSize(1) + objectRefSize(1)
    // + numObjects(8) + topObjectRef(8) + offsetTableOffset(8)
    const trailer = buf.length - 32
    const offsetIntSize = buf.readUInt8(trailer + 6)
    const objectRefSize = buf.readUInt8(trailer + 7)
    const topRef = readUint(buf, trailer + 16, 8)
    const tableOff = readUint(buf, trailer + 24, 8)

    const objOff = (ref) => readUint(buf, tableOff + ref * offsetIntSize, offsetIntSize)

    // 长度字段：marker 低 4 位 < 0xF 时即为长度；等于 0xF 时紧随一个整数对象
    // （marker 0x1x，占 2^(低4位) 字节）表示长度。
    function readLen(off) {
      let len = buf.readUInt8(off) & 0x0f
      let p = off + 1
      if (len === 0x0f) {
        const size = 1 << (buf.readUInt8(p) & 0x0f)
        len = readUint(buf, p + 1, size)
        p += 1 + size
      }
      return { len, dataOff: p }
    }

    function readValue(ref) {
      const off = objOff(ref)
      const marker = buf.readUInt8(off)
      switch (marker >> 4) {
        case 0x5: {
          // ASCII 字符串
          const { len, dataOff } = readLen(off)
          return buf.toString('latin1', dataOff, dataOff + len)
        }
        case 0x6: {
          // UTF-16BE 字符串。Node 没有 utf16be 编码：先复制成独立 Buffer（不能
          // 直接对 subarray 调 swap16 —— 那会原地改写整个 buf，破坏后续解析），
          // 再 swap16() 转成小端后用 utf16le 解码。
          const { len, dataOff } = readLen(off)
          const bytes = Buffer.from(buf.subarray(dataOff, dataOff + len * 2))
          return bytes.swap16().toString('utf16le')
        }
        case 0x1: {
          // 整数
          const size = 1 << (marker & 0x0f)
          return String(readUint(buf, off + 1, size))
        }
        case 0x2: {
          // 实数
          const size = 1 << (marker & 0x0f)
          return size === 8 ? String(buf.readDoubleBE(off + 1)) : String(buf.readFloatBE(off + 1))
        }
        default:
          return null
      }
    }

    const off = objOff(topRef)
    if (buf.readUInt8(off) >> 4 !== 0xd) return null // 顶层不是字典
    const { len, dataOff } = readLen(off)
    const out = {}
    for (let i = 0; i < len; i++) {
      // 字典布局：先 count 个 key 引用，再 count 个 value 引用
      const kRef = readUint(buf, dataOff + i * objectRefSize, objectRefSize)
      const vRef = readUint(buf, dataOff + (len + i) * objectRefSize, objectRefSize)
      const k = readValue(kRef)
      const v = readValue(vRef)
      if (typeof k === 'string' && v != null) out[k] = v
    }
    return out
  } catch {
    return null
  }
}

/**
 * 读取 plist 文件里的版本槽位（自动区分 XML / 二进制形态）。
 *
 * @param {string} plist plist 文件路径
 * @returns {{kind:'xml'|'binary'|'unknown', short:string|null, build:string|null}}
 *   short = CFBundleShortVersionString，build = CFBundleVersion
 */
export function readPlistVersion(plist) {
  const buf = fs.readFileSync(plist)

  if (buf.toString('latin1', 0, 8) === BPLIST_MAGIC) {
    const d = readBinaryPlist(buf)
    return {
      kind: 'binary',
      short: d?.CFBundleShortVersionString ?? null,
      build: d?.CFBundleVersion ?? null,
    }
  }

  const t = buf.toString('utf8')
  if (t.includes('<plist')) {
    const pick = (k) => t.match(new RegExp(`<key>${k}</key>\\s*<string>([^<]*)</string>`))?.[1] ?? null
    return { kind: 'xml', short: pick('CFBundleShortVersionString'), build: pick('CFBundleVersion') }
  }

  return { kind: 'unknown', short: null, build: null }
}
