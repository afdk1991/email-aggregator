/**
 * ============================================================================
 *  归档内文搜索（apk / aab / ipa 都是 ZIP）
 * ============================================================================
 *
 *  为什么不能直接对文件原文做 `buf.includes(needle)`：
 *    APK / AAB / IPA 都是 ZIP。条目默认用 **deflate** 压缩，而版本号恰好只出现在
 *    文本条目里（`assets/public/index.html` 的 `<meta name="app-version">`、
 *    打进 bundle 的 `version.generated.ts`…）。这些文本被压缩后，明文字符串
 *    在文件原文中**通常不再连续出现** —— 直接搜原文会得到**假阴性**，
 *    表现为"包明明打对了却报未检出版本号"。
 *
 *  本模块先走原文快路径（stored 条目、ZIP 中央目录里的条目名等仍可能命中），
 *  未命中时再按中央目录逐条目解压后搜索。纯 Node 内置模块，无第三方依赖。
 *
 *  性能约束：未命中时要遍历整包，所以对条目做**体积上限**过滤 —— 版本号这类
 *  文本只会出现在 index.html / JS bundle 这种小条目里，不可能藏在一个几 MB 的
 *  .so 或 .dex 中。默认跳过解压后 > 8 MiB 的条目，把一次全盘扫描从"几十秒"
 *  压回"一两秒"量级。
 * ============================================================================
 */

import fs from 'node:fs'
import zlib from 'node:zlib'

const EOCD_SIG = 0x06054b50 // 中央目录结束记录 PK\x05\x06
const CEN_SIG = 0x02014b50 // 中央目录条目   PK\x01\x02
const LOC_SIG = 0x04034b50 // 本地文件头     PK\x03\x04

/** 从尾部定位 EOCD（注释区最长 65535 字节）。 */
function findEocd(buf) {
  const min = Math.max(0, buf.length - 22 - 0xffff)
  for (let i = buf.length - 22; i >= min; i--) {
    if (buf.readUInt32LE(i) === EOCD_SIG) return i
  }
  return -1
}

/** 解析中央目录，返回条目列表。非 ZIP 或结构异常时返回 null。 */
export function readZipEntries(buf) {
  const eocd = findEocd(buf)
  if (eocd < 0) return null
  const total = buf.readUInt16LE(eocd + 10)
  let off = buf.readUInt32LE(eocd + 16)
  const entries = []
  for (let n = 0; n < total; n++) {
    if (off + 46 > buf.length || buf.readUInt32LE(off) !== CEN_SIG) break
    entries.push({
      name: buf.subarray(off + 46, off + 46 + buf.readUInt16LE(off + 28)).toString('utf8'),
      method: buf.readUInt16LE(off + 10),
      flags: buf.readUInt16LE(off + 8),
      compSize: buf.readUInt32LE(off + 20),
      unCompSize: buf.readUInt32LE(off + 24),
      localOff: buf.readUInt32LE(off + 42),
    })
    off += 46 + buf.readUInt16LE(off + 28) + buf.readUInt16LE(off + 30) + buf.readUInt16LE(off + 32)
  }
  return entries
}

/** 取出单个条目的解压后字节。取不到时返回 null。 */
export function readZipEntry(buf, e) {
  if (e.localOff + 30 > buf.length || buf.readUInt32LE(e.localOff) !== LOC_SIG) return null
  const start = e.localOff + 30 + buf.readUInt16LE(e.localOff + 26) + buf.readUInt16LE(e.localOff + 28)
  const raw = buf.subarray(start, start + e.compSize)
  if (e.method === 0) return raw // stored
  if (e.method === 8) {
    try {
      return zlib.inflateRawSync(raw)
    } catch {
      return null
    }
  }
  return null // 其它压缩方式（bzip2/lzma）极少见，跳过
}

/**
 * 在归档（或其解压内容）中查找字节串。
 *
 * @param {string} file 归档路径
 * @param {Buffer|string} needle 要查找的内容
 * @param {{maxEntryBytes?:number}} [opts] maxEntryBytes 默认 8 MiB；
 *   解压后超过该体积的条目直接跳过（见文件头「性能约束」）。
 * @returns {{hit:boolean, how:string|null, where:string[], scanned:number}}
 *   how: 'raw'（原文命中，可能是 stored 条目）| 'zip'（解压后命中）| null
 *   where: 命中的条目名（最多 5 个），便于定位
 *   scanned: 实际解压检查过的条目数（排查假阴性时有用）
 */
export function archiveContains(file, needle, opts = {}) {
  const maxEntryBytes = opts.maxEntryBytes ?? 8 * 1024 * 1024
  const n = Buffer.isBuffer(needle) ? needle : Buffer.from(needle, 'utf8')
  const buf = fs.readFileSync(file)

  if (buf.includes(n)) return { hit: true, how: 'raw', where: [], scanned: 0 }

  const entries = readZipEntries(buf)
  if (!entries) return { hit: false, how: null, where: [], scanned: 0 }

  const where = []
  let scanned = 0
  for (const e of entries) {
    // 体积上限：大二进制条目不可能承载版本号文本，跳过以省下解压开销。
    if (e.unCompSize > maxEntryBytes) continue
    const data = readZipEntry(buf, e)
    if (!data) continue
    scanned++
    if (data.includes(n)) {
      where.push(e.name)
      if (where.length >= 5) break
    }
  }
  return { hit: where.length > 0, how: where.length ? 'zip' : null, where, scanned }
}
