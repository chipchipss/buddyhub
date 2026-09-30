/* ══════════════════════════════════════════════════════════════════
   qr.js · 精简 QR 编码器（券码二维码用）
   规格子集：byte 模式、ECC L、**版本 1-10**、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响可扫描性；
   已用 python qrcode 库对 v1–v10 逐版本做逐像素交叉验证（10/10 diff=0）。
   面板 CSP 只允许 self，外链 QR 服务不可用，故自带编码器。

   容量上限 271 字节（v10-L：274 数据码字 − 3 字节头）。超了 qrMatrix 抛错，
   调用方应退化成「显示链接 + 复制」而不是白屏——微信 OAuth 的授权链接
   有 220+ 字节，正好落在 v10 里。

   三处只有到 v6+/v7+/v10 才会暴露的坑（都踩过，别再改回去）：
     - v6+ 是多纠错块，码字必须**交织**（v1–5 单块才免交织）
     - v7+ 有版本信息（18 位），左下那份的位序与右上**互为转置**
     - v10+ 的字节模式**计数指示符是 16 位**（v1–9 是 8 位）
   ══════════════════════════════════════════════════════════════════ */

// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-10、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1），ECC L：
//   [数据码字总数, 每块纠错码字数, [[块数, 每块数据码字数], …]]
// v1–v5 单块（免交织）；v6+ 多块，码字需按块交织（见 qrCodewords）。
// 最后一组的每块数据码字比前一组多 1，这是 QR 规范的分组方式。
const QR_V = [
  [19, 7, [[1, 19]]],
  [34, 10, [[1, 34]]],
  [55, 15, [[1, 55]]],
  [80, 20, [[1, 80]]],
  [108, 26, [[1, 108]]],
  [136, 18, [[2, 68]]],
  [156, 20, [[2, 78]]],
  [194, 24, [[2, 97]]],
  [232, 30, [[2, 116]]],
  [274, 18, [[2, 68], [2, 69]]],
];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34], [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50]];
// 版本信息（v7+ 才有）：18 位，规范附表固定值，直接查表比现算 BCH 不易出错。
const QR_VERSION_INFO = { 7: 0x07C94, 8: 0x085BC, 9: 0x09A99, 10: 0x0A4D3 };
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 计数 + 数据 + 终止符 + 0xEC/0x11 填充）
//
// 计数指示符的位宽**随版本变**：v1–9 是 8 位，v10–26 是 16 位。
// 写死 8 位时 v10 起会整体错位——前几个码字看着还对，后面全是错的。
function qrDataCodewords(text, dataCap, ver) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);                          // byte 模式
  push(bytes.length, ver >= 10 ? 16 : 8); // v1-9: 8 位；v10-26: 16 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 数据码字 → 分块 + 纠错 + 交织（v6+ 多块必须交织，否则解码器读不出来）
function qrCodewords(text, dataCap, ecPerBlock, groups, ver) {
  const data = qrDataCodewords(text, dataCap, ver);
  const blocks = [];
  let pos = 0;
  for (const [count, size] of groups) {
    for (let i = 0; i < count; i++) {
      const blk = data.slice(pos, pos + size);
      pos += size;
      blocks.push({ data: blk, ec: rsRem(blk, ecPerBlock) });
    }
  }
  // 交织：先按列取各块的数据码字，再按列取各块的纠错码字
  const out = [];
  const maxData = Math.max(...blocks.map(b => b.data.length));
  for (let i = 0; i < maxData; i++) for (const b of blocks) if (i < b.data.length) out.push(b.data[i]);
  for (let i = 0; i < ecPerBlock; i++) for (const b of blocks) out.push(b.ec[i]);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
export function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 = 模式(4位) + 计数(v1-9 8位 / v10+ 16位) + 数据，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) {
    const overhead = v + 1 >= 10 ? 3 : 2; // 计数位宽换算成字节数（向上取整后）
    if (bytes.length + overhead <= QR_V[v][0]) { ver = v + 1; break; }
  }
  if (!ver) throw new Error('QR: text too long (>' + (QR_V[QR_V.length - 1][0] - 2) + ' bytes)');
  const [dataCap, ecPerBlock, groups] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+）。**只跳过三个角**（与定位图案重叠的那三个），
  // 判据是**下标**而不是「中心格已被占」——中心落在时序行/列上的那两个
  // （如 v7 的 (6,22)、(22,6)）必须照放：按格占用判会误跳它们，
  // 少掉 2×25−10=40 个功能模块，数据流整体错位、扫出来是乱码。
  const align = QR_ALIGN[ver - 1] || [];
  const lastA = align.length - 1;
  for (let i = 0; i < align.length; i++) for (let j = 0; j < align.length; j++) {
    if ((i === 0 && j === 0) || (i === 0 && j === lastA) || (i === lastA && j === 0)) continue;
    const ar = align[i], ac = align[j];
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);
  // 版本信息（v7+）：18 位，右上与左下各放一份
  const vinfo = QR_VERSION_INFO[ver];
  if (vinfo !== undefined) {
    for (let i = 0; i < 18; i++) {
      const bit = ((vinfo >> i) & 1) === 1;
      // 右上：6 行 × 3 列（行 0..5，列 n-11..n-9）
      setF(Math.floor(i / 3), n - 11 + (i % 3), bit);
      // 左下：3 行 × 6 列（行 n-11..n-9，列 0..5）。
      // 注意位序与右上**互为转置**：bit i → 行 n-11+(i%3)、列 i/3。
      // 写成 i/6、i%6 会得到同样的格子但位错位，扫描器读出来是错的版本号。
      setF(n - 11 + (i % 3), Math.floor(i / 3), bit);
    }
  }


  // 数据码字 + 纠错码字 → 位流
  const cw = qrCodewords(text, dataCap, ecPerBlock, groups, ver);
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG 元素（quiet zone 4 模块）。
//
// 返回**真实 SVG 元素**而不是标记字符串：h() 把字符串当文本节点处理，
// 传标记进去只会把 "<svg ...>" 原样显示在页面上（二维码功能会静默失效）。
//
// 深色模块合成单条 <path>，而不是每格一个 <rect>——v5 有约 600 个深色模块，
// 一个 path 既省 DOM 也省序列化体积。
export function qrSVG(M, px) {
  const NS = 'http://www.w3.org/2000/svg';
  const n = M.length, q = 4, total = n + q * 2;
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 ' + total + ' ' + total);
  svg.setAttribute('width', px);
  svg.setAttribute('height', px);
  svg.setAttribute('shape-rendering', 'crispEdges');
  svg.setAttribute('role', 'img');
  svg.setAttribute('style', 'background:#fff');

  const d = [];
  for (let r = 0; r < n; r++) {
    for (let c = 0; c < n; c++) {
      if (M[r][c]) d.push('M' + (c + q) + ' ' + (r + q) + 'h1v1h-1z');
    }
  }
  const path = document.createElementNS(NS, 'path');
  path.setAttribute('d', d.join(''));
  path.setAttribute('fill', '#000');
  svg.appendChild(path);
  return svg;
}
