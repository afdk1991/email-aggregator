#!/usr/bin/env node
/**
 * ============================================================================
 *  移动端校验脚本的自测（纯 Node，零第三方依赖）
 * ============================================================================
 *
 *  守的是**两个静默假阴性陷阱** —— 它们不会报错、只会"看起来通过"或
 *  "明明对了却判失败"，靠读代码很难发现，必须用夹具钉死：
 *
 *    ① APK / AAB / IPA 都是 ZIP，文本条目默认 deflate 压缩。
 *       对文件原文 `buf.includes('MALLV0.0.0')` 会得到 false。
 *       本自测先断言这个 false **确实存在**（证明确有陷阱），
 *       再断言 archiveContains() 能解压后命中。
 *
 *    ② Xcode 会把 Info.plist 重新序列化成**二进制 plist（bplist00）**，
 *       文本正则匹配 `<key>CFBundleVersion</key>` 匹配不到。
 *       本自测用一段内嵌的 bplist 夹具，断言 readPlistVersion() 能读出
 *       两个版本键，且非 ASCII 值（UTF-16BE 分支）也不出错。
 *
 *  ZIP 夹具由本文件内的最小 ZIP writer 现造（deflateRaw + 中央目录 + EOCD），
 *  所以不往仓库里塞二进制文件，也不依赖 Python / zip 命令。
 *
 *  用法：node scripts/selftest.mjs
 *  退出码：0 全通过；1 有失败
 * ============================================================================
 */

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import zlib from 'node:zlib'

import { readPlistVersion } from './plist.mjs'
import { archiveContains } from './zip-search.mjs'

const checks = []
const check = (name, cond, detail) => {
  checks.push({ ok: !!cond, name, detail })
  console.log(`  ${cond ? 'PASS' : 'FAIL'}  ${name}\n        ${detail}`)
}

// ───────────────────────────────────────────────────────────────────────────
//  夹具 ①：最小 ZIP writer
// ───────────────────────────────────────────────────────────────────────────

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
  let crc = -1
  for (let i = 0; i < buf.length; i++) crc = (crc >>> 8) ^ CRC_TABLE[(crc ^ buf[i]) & 0xff]
  return (crc ^ -1) >>> 0
}

/**
 * 造一个最小但结构合法的 ZIP（无 ZIP64、无数据描述符、无加密）。
 * @param {{name:string, data:Buffer, store?:boolean}[]} entries
 */
function makeZip(entries) {
  const locals = []
  const centrals = []
  let offset = 0

  for (const e of entries) {
    const nameBuf = Buffer.from(e.name, 'utf8')
    const method = e.store ? 0 : 8
    const body = method === 0 ? e.data : zlib.deflateRawSync(e.data)
    const crc = crc32(e.data)

    const lh = Buffer.alloc(30)
    lh.writeUInt32LE(0x04034b50, 0) // 本地文件头签名
    lh.writeUInt16LE(20, 4) // version needed
    lh.writeUInt16LE(0, 6) // flags
    lh.writeUInt16LE(method, 8)
    lh.writeUInt32LE(crc, 14)
    lh.writeUInt32LE(body.length, 18)
    lh.writeUInt32LE(e.data.length, 22)
    lh.writeUInt16LE(nameBuf.length, 26)
    locals.push(lh, nameBuf, body)

    const ch = Buffer.alloc(46)
    ch.writeUInt32LE(0x02014b50, 0) // 中央目录条目签名
    ch.writeUInt16LE(20, 4) // version made by
    ch.writeUInt16LE(20, 6) // version needed
    ch.writeUInt16LE(method, 10)
    ch.writeUInt32LE(crc, 16)
    ch.writeUInt32LE(body.length, 20)
    ch.writeUInt32LE(e.data.length, 24)
    ch.writeUInt16LE(nameBuf.length, 28)
    ch.writeUInt32LE(offset, 42) // 本地文件头偏移
    centrals.push(ch, nameBuf)

    offset += lh.length + nameBuf.length + body.length
  }

  const cd = Buffer.concat(centrals)
  const eocd = Buffer.alloc(22)
  eocd.writeUInt32LE(0x06054b50, 0) // 中央目录结束记录签名
  eocd.writeUInt16LE(entries.length, 8) // 本盘条目数
  eocd.writeUInt16LE(entries.length, 10) // 总条目数
  eocd.writeUInt32LE(cd.length, 12)
  eocd.writeUInt32LE(offset, 16) // 中央目录偏移

  return Buffer.concat([...locals, cd, eocd])
}

// ───────────────────────────────────────────────────────────────────────────
//  夹具 ②：二进制 plist（bplist00）—— plistlib 生成后内嵌 base64
//
//  内容：CFBundleShortVersionString=0.0.0 / CFBundleVersion=1 /
//        CFBundleName=EmailAggregator / CFBundleDisplayName=邮箱聚合
//  刻意带一个非 ASCII 值，用来覆盖读取器的 UTF-16BE 分支。
// ───────────────────────────────────────────────────────────────────────────
const BPLIST_FIXTURE_B64 =
  'YnBsaXN0MDDUAQIDBAUGBwhfEBNDRkJ1bmRsZURpc3BsYXlOYW1lXENGQnVuZGxlTmFtZV8QGkNG' +
  'QnVuZGxlU2hvcnRWZXJzaW9uU3RyaW5nXxAPQ0ZCdW5kbGVWZXJzaW9uZJCue7GAWlQIXxAPRW1h' +
  'aWxBZ2dyZWdhdG9yVTAuMC4wUTEIESc0UWNsfoQAAAAAAAABAQAAAAAAAAAJAAAAAAAAAAAAAAAA' +
  'AAAAhg=='

const XML_PLIST_FIXTURE = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleShortVersionString</key><string>0.0.0</string>
  <key>CFBundleVersion</key><string>1</string>
</dict></plist>
`

// ───────────────────────────────────────────────────────────────────────────

function main() {
  console.log('[selftest] 移动端校验脚本回归自测\n')

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'p003-selftest-'))

  try {
    // ── ① ZIP 压缩假阴性 ────────────────────────────────────────────────
    console.log('① ZIP 压缩假阴性（apk / aab / ipa 都是 ZIP）')

    const needle = 'MALLV0.0.0'
    // 版本号出现在这类文本条目里：assets/public/index.html（Capacitor 的 webDir 落点）
    const html = Buffer.from('<html><head><meta name="app-version" content="MALLV0.0.0" /></head></html>', 'utf8')

    //  注意夹具设计：这个包里**所有**条目都必须经过压缩，否则未压缩（stored）条目
    //  会让明文直接出现在文件原文里，原文快路径就会命中，测不出假阴性。
    //  （第一版本的自测就在这里自己抓到了夹具缺陷：多加了一个 stored 条目后
    //   how 变成了 'raw'，陷阱验不出来。stored 分支单独用下面的 zipB 覆盖。）
    const zipPath = path.join(tmp, 'fixture.apk')
    const zipBuf = makeZip([
      { name: 'assets/public/index.html', data: html },
      { name: 'META-INF/MANIFEST.MF', data: Buffer.from('Created-By: selftest\n') },
      { name: 'classes.dex', data: Buffer.from('not really dex', 'utf8') },
    ])
    fs.writeFileSync(zipPath, zipBuf)

    check(
      '夹具本身是压缩的：原文搜不到版本号（证明陷阱真实存在）',
      !zipBuf.includes(Buffer.from(needle, 'utf8')),
      `buf.includes(${JSON.stringify(needle)}) = false —— 旧写法 fs.readFileSync().includes() 在此必然假阴性`,
    )

    const hit = archiveContains(zipPath, needle)
    check(
      'deflate 条目：解压后能命中',
      hit.hit && hit.how === 'zip' && hit.where.includes('assets/public/index.html'),
      `how=${hit.how}, where=[${hit.where.join(', ')}], scanned=${hit.scanned}`,
    )

    // stored（未压缩）条目：走原文快路径即可命中，不必解压
    const storedPath = path.join(tmp, 'stored.apk')
    fs.writeFileSync(storedPath, makeZip([{ name: 'assets/public/index.html', data: html, store: true }]))
    const storedHit = archiveContains(storedPath, needle)
    check(
      'stored 条目：走原文快路径命中',
      storedHit.hit && storedHit.how === 'raw',
      `how=${storedHit.how}, hit=${storedHit.hit}`,
    )

    const miss = archiveContains(zipPath, 'MALLV9.9.9')
    check('负例：不存在的版本号不误报', !miss.hit, `hit=${miss.hit}, scanned=${miss.scanned}`)

    const nonZip = path.join(tmp, 'not-a-zip.bin')
    fs.writeFileSync(nonZip, Buffer.from('plain text, no EOCD', 'utf8'))
    const notZip = archiveContains(nonZip, needle)
    check('非 ZIP 文件：安全返回未命中而不是抛异常', !notZip.hit, `hit=${notZip.hit}, how=${notZip.how}`)

    const bigPath = path.join(tmp, 'big-entry.apk')
    fs.writeFileSync(
      bigPath,
      makeZip([{ name: 'libhuge.so', data: Buffer.concat([Buffer.alloc(9 * 1024 * 1024, 0x41), html]) }]),
    )
    const skipped = archiveContains(bigPath, needle)
    check(
      '体积上限：> 8 MiB 的条目被跳过（不因大 .so/.dex 拖慢扫描）',
      !skipped.hit && skipped.scanned === 0,
      `hit=${skipped.hit}, scanned=${skipped.scanned}（预期 0，该条目被跳过）`,
    )

    // ── ② 二进制 plist 假阴性 ───────────────────────────────────────────
    console.log('\n② 二进制 plist 假阴性（Xcode 重序列化后的 Info.plist）')

    const bplist = path.join(tmp, 'Info.plist')
    const bplistBuf = Buffer.from(BPLIST_FIXTURE_B64, 'base64')
    fs.writeFileSync(bplist, bplistBuf)

    check(
      '夹具确实是二进制 plist',
      bplistBuf.toString('latin1', 0, 8) === 'bplist00',
      `magic=${JSON.stringify(bplistBuf.toString('latin1', 0, 8))}, ${bplistBuf.length} 字节`,
    )
    check(
      '夹具里确实没有 XML 标签（文本正则必然假阴性）',
      !bplistBuf.toString('utf8').includes('<key>CFBundleVersion</key>'),
      '文本正则匹配 <key>CFBundleVersion</key> 会得到 null',
    )

    const got = readPlistVersion(bplist)
    check(
      'readPlistVersion 能读出两个版本键',
      got.kind === 'binary' && got.short === '0.0.0' && got.build === '1',
      `kind=${got.kind}, short=${got.short}, build=${got.build}`,
    )

    const wrong = path.join(tmp, 'wrong.plist')
    fs.writeFileSync(wrong, XML_PLIST_FIXTURE.replace('<string>1</string>', '<string>2</string>'), 'utf8')
    const wrongGot = readPlistVersion(wrong)
    check(
      '负例：CFBundleVersion 不符时读到的是实际值（不是期望值）',
      wrongGot.build === '2' && wrongGot.build !== '1',
      `实际读到 build=${wrongGot.build}（构造值 2，期望值 1）`,
    )

    const xmlPath = path.join(tmp, 'xml.plist')
    fs.writeFileSync(xmlPath, XML_PLIST_FIXTURE, 'utf8')
    const xmlGot = readPlistVersion(xmlPath)
    check(
      'XML plist 分支同样可用',
      xmlGot.kind === 'xml' && xmlGot.short === '0.0.0' && xmlGot.build === '1',
      `kind=${xmlGot.kind}, short=${xmlGot.short}, build=${xmlGot.build}`,
    )

    const junkPath = path.join(tmp, 'junk.plist')
    fs.writeFileSync(junkPath, 'not a plist at all', 'utf8')
    const junk = readPlistVersion(junkPath)
    check(
      '非 plist 文件：返回 unknown 而不抛异常',
      junk.kind === 'unknown' && junk.short === null,
      `kind=${junk.kind}, short=${junk.short}`,
    )
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }

  const failed = checks.filter((c) => !c.ok)
  console.log('')
  if (failed.length) {
    console.log(`[selftest] ${failed.length}/${checks.length} 项失败`)
    process.exit(1)
  }
  console.log(`[selftest] ${checks.length}/${checks.length} 项全部通过`)
}

main()
