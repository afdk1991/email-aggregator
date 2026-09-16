// 文档引用有效性检测 v2 —— 用全局 basename 索引消除假阳性
// 区分三类：
//   A. 路径写法不完整，但同名文件在项目内存在 → 非问题（仅提示）
//   B. 项目内完全找不到该文件 → 需判断：历史记录（可接受） or 文档腐化（需修）
//   C. 指向构建产物/临时文件 → 本就不该长期存在
import fs from 'node:fs';
import path from 'node:path';

const ROOT = 'D:/网站全栈项目/项目003';
const SKIP = new Set(['node_modules', '.gopath', '.gocache', '.git']);

function walk(dir, out = []) {
  let ents;
  try { ents = fs.readdirSync(dir, { withFileTypes: true }); } catch { return out; }
  for (const e of ents) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) { if (SKIP.has(e.name)) continue; walk(p, out); }
    else out.push(p);
  }
  return out;
}
const rel = (p) => path.relative(ROOT, p).replace(/\\/g, '/');
const all = walk(ROOT);

// basename 索引（含带目录的相对路径，用于模糊匹配）
const byBase = new Map();
for (const f of all) {
  const r = rel(f), b = path.basename(f);
  if (!byBase.has(b)) byBase.set(b, []);
  byBase.get(b).push(r);
}

const mdFiles = all.filter((f) => f.endsWith('.md'));
// 只检测「当前状态描述型」文档的引用；历史日志与审计报告引用已删文件属正常
const pathRe = /`([A-Za-z0-9_@./\\-]+\.(?:md|go|ts|tsx|js|mjs|json|yml|yaml|ps1|sh|css|html|sql|mod|toml))`/g;

const rows = [];
let checked = 0;
for (const md of mdFiles) {
  const text = fs.readFileSync(md, 'utf8');
  const mdDir = path.dirname(md);
  const seen = new Set();
  for (const m of text.matchAll(pathRe)) {
    const ref = m[1].replace(/\\/g, '/');
    if (ref.includes('*') || ref.startsWith('http') || seen.has(ref)) continue;
    seen.add(ref);
    checked++;
    const cands = [
      path.join(mdDir, ref), path.join(ROOT, ref),
      path.join(ROOT, 'email-aggregator-go', ref),
      path.join(ROOT, 'email-aggregator-web', ref),
      path.join(ROOT, 'email-aggregator', ref),
    ];
    if (cands.some((c) => fs.existsSync(c))) continue; // 精确命中，OK

    const base = path.basename(ref);
    const hits = byBase.get(base) || [];
    rows.push({
      doc: rel(md),
      ref,
      base,
      hits,
      cls: hits.length ? 'A' : 'B',
    });
  }
}

const clsA = rows.filter((r) => r.cls === 'A');
const clsB = rows.filter((r) => r.cls === 'B');

console.log(`检测唯一引用 ${checked} 处`);
console.log(`  精确命中        : ${checked - rows.length}`);
console.log(`  A 类（同名文件存在，仅路径写法不完整）: ${clsA.length}`);
console.log(`  B 类（项目内确实不存在该文件）        : ${clsB.length}`);

// B 类按 basename 聚合，判断是否属"历史已删除"（docs 里说的是过去删掉的文件）
console.log('\n=== B 类明细（项目内不存在）===');
const grouped = {};
for (const r of clsB) (grouped[r.base] ||= []).push(r.doc);
for (const [base, docs] of Object.entries(grouped).sort((a, b) => b[1].length - a[1].length)) {
  const uniq = [...new Set(docs)];
  console.log(`  ✗ ${base}  ← ${uniq.length} 份文档`);
  for (const d of uniq.slice(0, 3)) console.log(`        ${d}`);
  if (uniq.length > 3) console.log(`        …另 ${uniq.length - 3} 份`);
}

// 只看近期文档（9 月）的 B 类 —— 这些才是真正需要处理的文档腐化
console.log('\n=== 近期文档（2026-09 修改）的 B 类引用 ===');
const recent = clsB.filter((r) => {
  const p = path.join(ROOT, r.doc);
  try { return fs.statSync(p).mtime.toISOString().slice(0, 7) === '2026-09'; } catch { return false; }
});
if (!recent.length) console.log('  无');
else {
  const g2 = {};
  for (const r of recent) (g2[r.doc] ||= []).push(r.ref);
  for (const [doc, refs] of Object.entries(g2)) {
    console.log(`  ${doc}`);
    for (const x of [...new Set(refs)].sort()) console.log(`      ✗ ${x}`);
  }
}

// A 类：文档写的路径与真实位置不符（可批量修正，属易读性问题）
console.log('\n=== A 类：路径写法与真实位置不符（TOP 20）===');
const g3 = {};
for (const r of clsA) (g3[r.ref] ||= { doc: r.doc, hits: r.hits });
for (const [ref, v] of Object.entries(g3).slice(0, 20)) {
  console.log(`  ~ ${ref}`);
  console.log(`        实际存在于: ${v.hits.slice(0, 2).join(' | ')}`);
}
