// models_render_test.go 「模型与档位」这一屏必须真的能挑能复制（清单 43/44）。
//
// 为什么需要：这页是命令式建表 + 就地重画（搜索/筛选/排序只换表体，不重建整屏，
// 否则正在打字的搜索框会丢焦点）。「表头点了没反应」「筛完计数还在说全部」「点名字
// 复制到的不是能直接用的那个串」都是编译与 import 冒烟拦不住、用户第一天就撞的错。
// 放在独立文件里，共用 frontend_test.go 的桩与脚手架。无 node 时跳过。
package panel

import (
	"encoding/json"
	"os/exec"
	"testing"
)

func TestModelsPageRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; render check skipped")
	}
	dir := copyModules(t)

	// 排序发生在平台分组内部（各平台分开成表），所以同一平台要有几个价格不同的模型，
	// 否则「排完序第一行变了没」这种断言会因每组只有一行而恒真。
	models := []map[string]any{
		{"id": "cn:wb-free", "name": "免费版", "credits": "x0", "context_length": 8000, "max_output_tokens": 1024},
		{"id": "cn:wb-mid", "name": "主力", "credits": "x1.2", "context_length": 128000, "max_output_tokens": 4096,
			"is_default": true, "supports_tool_call": true},
		{"id": "cn:wb-promo", "name": "限时款", "credits": "x2", "promo_credits": "0.5x", "promo_label": "限时半价",
			"promo_factor": 0.5, "context_length": 32000, "max_output_tokens": 2048, "supports_images": true},
		{"id": "zai:glm-air", "name": "GLM 轻量", "credits": "x0.5", "context_length": 64000, "max_output_tokens": 4096},
	}
	list, err2 := json.Marshal(models)
	if err2 != nil {
		t.Fatal(err2)
	}

	harness := wizardBoot(t) + `
const MODELS = ` + string(list) + `;
// 复制走安全上下文的 navigator.clipboard：桩里没有可用的 navigator（Node 自带那个
// 是只读访问器），所以 defineProperty 换掉——断言时才看得到「到底复制了什么串」。
const copied = [];
Object.defineProperty(globalThis, 'navigator', { configurable: true, writable: true,
  value: { clipboard: { writeText: t2 => { copied.push(t2); return Promise.resolve(); } } } });
window.isSecureContext = true;
DATA['/api/models'] = { ok: true, models: MODELS };
DATA['/api/model_probes'] = { ok: true, probes: {} };

const view = (await import('./views/models.js')).default;
const mount = document.createElement('div');
document.body.append(mount);
let root = null;
const paint = () => { root = view.render(); mount.replaceChildren(root); };

paint();
await tick(40);
paint();
await tick(40);   // 表体是微任务里重画的，等它填上

const tbodies = () => tags(root, 'tbody');
const theads = () => tags(root, 'thead');
// findAll 按类名找，thead/tbody 没有类名——标签得用这个走 tagName 的遍历
const tags = (el, tag, out = []) => {
  for (const c of el.children || []) {
    if ((c.tagName || '').toLowerCase() === tag) out.push(c);
    tags(c, tag, out);
  }
  return out;
};
if (!theads().length) {
  console.error('DIAG calls=' + JSON.stringify(CALLS));
  console.error('DIAG body=' + root.textContent.slice(0, 200));
  bad('首屏不是表格（load 没完成或走错分支）');
}
const wbRows = () => (tbodies()[0] || { children: [] }).children.map(r => r.textContent);
const numHeads = () => theads()[0].children[0].children.filter(th => th.classList.contains('num'));
const allHeads = () => theads()[0].children[0].children.map(th => th.textContent.trim());
const segWith = kw => findAll(document.body, 'seg').find(s => s.textContent.includes(kw));

/* ── 1. 表头：可排序的三列 + 能力单独成列（清单 43）──────────── */
if (!allHeads().some(x => x.startsWith('倍率'))) bad('倍率列不可排序：' + allHeads().join(' | '));
if (!allHeads().includes('能力')) bad('能力徽章没单独成列（原来塞在模型名下面）：' + allHeads().join(' | '));
if (numHeads().length !== 3) bad('可排序的数字列应有 3 个，实为 ' + numHeads().length);
if (tbodies().length !== 2) bad('平台分组表数量不对：' + tbodies().length);

/* ── 2. 点模型名复制完整 ID（含前缀，清单 44）────────────────── */
const nameCell = tbodies()[0].children[0].children[0].children[0];
if (!nameCell.textContent.includes('cn:')) bad('模型名里没有前缀，复制出去不能直接用：' + nameCell.textContent);
await click(nameCell, '点模型名');
if (copied.length !== 1 || copied[0] !== nameCell.textContent) bad('点名字没复制这个串：' + JSON.stringify(copied));

/* ── 3. 排序真的在动数字，再点换方向 ────────────────────────── */
// 注意：默认清单第一行恰好就是 0 倍率的 wb-free，所以「第一行变没变」恒真/恒假
// 都不可靠——直接断言组内整段顺序（升序 0 → 0.5 → 1.2，降序倒过来）。
const wbIds = () => wbRows().map(r => (r.match(/cn:wb-\w+/) || ['?'])[0]);
await click(numHeads()[0], '按倍率升序');
if (wbIds().join(',') !== 'cn:wb-free,cn:wb-promo,cn:wb-mid') bad('升序没按生效价排（0/0.5/1.2）：' + wbIds().join(','));
if (!numHeads()[0].textContent.includes('▲')) bad('升序时表头没有方向指示');
await click(numHeads()[0], '再点换降序');
if (wbIds().join(',') !== 'cn:wb-mid,cn:wb-promo,cn:wb-free') bad('降序没倒过来：' + wbIds().join(','));
if (!numHeads()[0].textContent.includes('▼')) bad('降序时表头没有方向指示');

/* ── 4. 只看免费 / 打折中：筛完要说「筛出 X / N 个」─────────── */
const fseg = segWith('只看免费');
if (!fseg) bad('没有「全部模型 / 只看免费 / 打折中」筛选');
else {
  await click([...fseg.children].find(b => b.textContent.trim() === '只看免费'), '只看免费');
  if (tbodies().length !== 1) bad('只看免费后还剩两张表：' + tbodies().length);
  if (!wbRows()[0].includes('wb-free')) bad('免费筛错行：' + wbRows()[0].slice(0, 30));
  if (!root.textContent.includes('筛出 1 / 4 个')) bad('筛完计数还写着全部：' + root.textContent.slice(0, 80));

  const fseg2 = segWith('打折中');
  await click([...fseg2.children].find(b => b.textContent.trim() === '打折中'), '打折中');
  if (!wbRows()[0].includes('限时半价')) bad('打折中筛错行：' + wbRows()[0].slice(0, 30));

  const fseg3 = segWith('全部模型');
  await click([...fseg3.children].find(b => b.textContent.trim() === '全部模型'), '全部模型');
  if (root.textContent.includes('筛出')) bad('退回全部后计数没复原');
}

/* ── 5. 搜索就地生效：不清空、不丢已填的词，查不到给退路 ────── */
const sq = qsel(root, 'input.search');
if (!sq) bad('没有搜模型的输入框');
else {
  sq.value = 'glm';
  fire(sq, 'input');
  await tick(30);
  if (tbodies().length !== 1 || !tags(root, 'tbody')[0].textContent.includes('glm-air')) bad('搜索没缩小结果：' + root.textContent.slice(0, 60));
  if (sq.value !== 'glm') bad('就地重画把搜索词丢了（焦点同理会丢）');
  sq.value = 'zzz-not-a-model';
  fire(sq, 'input');
  await tick(30);
  const em = findByClass(root, 'empty');
  if (!em) bad('搜不到时没有空状态');
  else if (!em.textContent.includes('全部模型')) bad('空状态没给退路（该说怎么退出筛选）：' + em.textContent.slice(0, 60));
}

if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
console.log('MODELS OK');
setTimeout(() => process.exit(0), 50);
`
	runNodeHarness(t, node, dir, "models-page.mjs", harness, "MODELS OK", "模型页的排序/筛选/复制不符")
}
