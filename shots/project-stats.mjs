// 项目003 客观规模统计 + 文档引用有效性检测
import fs from 'node:fs';
import path from 'node:path';

const ROOT = 'D:/网站全栈项目/项目003';
const SKIP = new Set(['node_modules', '.gopath', '.gocache', '.git', 'dist', 'bin', 'release', '.ci-logs']);

function walk(dir, out = []) {
  let ents;
  try { ents = fs.readdirSync(dir, { withFileTypes: true }); } catch { return out; }
  for (const e of ents) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) {
      if (SKIP.has(e.name)) continue;
      walk(p, out);
    } else {
      out.push(p);
    }
  }
  return out;
}

function lines(p) {
  try { return fs.readFileSync(p, 'utf8').split('\n').length; } catch { return 0; }
}

const files = walk(ROOT);
const rel = (p) => path.relative(ROOT, p).replace(/\\/g, '/');

// ---------- 1. 三套实现规模 ----------
const impls = {
  'email-aggregator (TS PoC)': (r) => r.startsWith('email-aggregator/'),
  'email-aggregator-go (Go 主干)': (r) => r.startsWith('email-aggregator-go/'),
  'email-aggregator-web (前端)': (r) => r.startsWith('email-aggregator-web/'),
};

console.log('=== 1. 三套实现规模 ===');
for (const [name, pred] of Object.entries(impls)) {
  const sub = files.filter((f) => pred(rel(f)));
  const stat = (ext) => {
    const s = sub.filter((f) => f.endsWith(ext));
    return { n: s.length, l: s.reduce((a, f) => a + lines(f), 0) };
  };
  const go = stat('.go'), ts = stat('.ts'), tsx = stat('.tsx'), css = stat('.css'), ps1 = stat('.ps1'), sh = stat('.sh');
  console.log(`${name}`);
  console.log(`  文件总数 ${sub.length} | 总字节 ${(sub.reduce((a, f) => a + fs.statSync(f).size, 0) / 1024).toFixed(1)} KB`);
  console.log(`  .go  ${go.n} 文件 / ${go.l} 行`);
  console.log(`  .ts  ${ts.n} 文件 / ${ts.l} 行`);
  console.log(`  .tsx ${tsx.n} 文件 / ${tsx.l} 行`);
  console.log(`  .css ${css.n} 文件 / ${css.l} 行`);
  console.log(`  .ps1 ${ps1.n} | .sh ${sh.n}`);
}

// ---------- 2. Go 包数 & 测试文件 ----------
console.log('\n=== 2. Go 包与测试 ===');
const goFiles = files.filter((f) => f.endsWith('.go') && rel(f).startsWith('email-aggregator-go/'));
const goTest = goFiles.filter((f) => f.endsWith('_test.go'));
const goDirs = new Set(goFiles.map((f) => path.dirname(rel(f))));
const goLines = goFiles.reduce((a, f) => a + lines(f), 0);
const goTestLines = goTest.reduce((a, f) => a + lines(f), 0);
console.log(`Go 源码   : ${goFiles.length} 文件 / ${goLines} 行 / ${goDirs.size} 个目录`);
console.log(`Go 测试   : ${goTest.length} 文件 / ${goTestLines} 行`);
console.log(`测试占比  : ${((goTestLines / goLines) * 100).toFixed(1)}%`);

// 构建标签分布
const tagCount = {};
for (const f of goFiles) {
  const head = fs.readFileSync(f, 'utf8').slice(0, 400);
  for (const m of head.matchAll(/^\/\/go:build (.+)$/gm)) {
    for (const t of m[1].split(/[,\s]+/).filter((x) => x && x !== '&&' && x !== '||')) {
      tagCount[t] = (tagCount[t] || 0) + 1;
    }
  }
}
console.log('构建标签 :', Object.entries(tagCount).sort((a, b) => b[1] - a[1]).map(([k, v]) => `${k}(${v})`).join(' '));

// ---------- 3. 前端组件清单 ----------
console.log('\n=== 3. 前端源码清单 ===');
const webSrc = files.filter((f) => rel(f).startsWith('email-aggregator-web/src/'));
for (const f of webSrc.sort()) {
  console.log(`  ${String(lines(f)).padStart(5)} 行  ${String(fs.statSync(f).size).padStart(7)} B  ${rel(f)}`);
}

// ---------- 4. 文档引用有效性检测（文档腐化检查） ----------
console.log('\n=== 4. 文档引用有效性检测 ===');
const mdFiles = files.filter((f) => f.endsWith('.md'));
const pathRe = /`([A-Za-z0-9_@./\\-]+\.(?:md|go|ts|tsx|js|mjs|json|yml|yaml|ps1|sh|css|html|sql|mod|sum|toml|env|example))`/g;
const missing = [];
let checked = 0;
for (const md of mdFiles) {
  const text = fs.readFileSync(md, 'utf8');
  const mdDir = path.dirname(md);
  for (const m of text.matchAll(pathRe)) {
    const ref = m[1].replace(/\\/g, '/');
    if (ref.includes('*') || ref.startsWith('http')) continue;
    checked++;
    // 多候选位置：文档同目录、项目根、Go 根、web 根
    const cands = [
      path.join(mdDir, ref),
      path.join(ROOT, ref),
      path.join(ROOT, 'email-aggregator-go', ref),
      path.join(ROOT, 'email-aggregator-web', ref),
      path.join(ROOT, 'email-aggregator', ref),
    ];
    if (!cands.some((c) => fs.existsSync(c))) {
      missing.push({ doc: rel(md), ref });
    }
  }
}
console.log(`检测引用 ${checked} 处，失效 ${missing.length} 处`);
if (missing.length) {
  const byRef = {};
  for (const m of missing) { (byRef[m.ref] ||= []).push(m.doc); }
  for (const [ref, docs] of Object.entries(byRef).sort((a, b) => b[1].length - a[1].length)) {
    console.log(`  ✗ ${ref}`);
    console.log(`      ← 出现于 ${docs.length} 份文档: ${docs.slice(0, 4).join(' / ')}${docs.length > 4 ? ' …' : ''}`);
  }
}

// ---------- 5. 文档总量 ----------
console.log('\n=== 5. 文档总量 ===');
const docs = mdFiles.map((f) => ({ f, r: rel(f), b: fs.statSync(f).size })).sort((a, b) => b.b - a.b);
console.log(`Markdown ${docs.length} 份 / ${(docs.reduce((a, d) => a + d.b, 0) / 1024).toFixed(1)} KB`);
console.log('最大的 8 份:');
for (const d of docs.slice(0, 8)) console.log(`  ${(d.b / 1024).toFixed(1).padStart(7)} KB  ${d.r}`);
