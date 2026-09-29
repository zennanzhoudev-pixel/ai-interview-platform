// 检查"JS 用到的 class 是否都在样式表里有定义"。
//
// 为什么需要这个检查: 前端没有构建步骤, 也没有类型系统, 于是
// "JS 写了一个类名、样式表里却没有" 这类错误不会有任何提示 ——
// 它只会表现为"这一块长得不对": 步骤条的数字没有圆圈、提示文字挤成一句、
// 视频框带着旧定位盖住按钮。这些都属于"看截图才发现, 看代码看不出来"。
//
// 真实案例: 步骤条曾经用 .step-index, 而样式表里定义的是 .num,
// 结果序号没有样式。这类问题的修法不是"再写一条 CSS", 而是让机器发现它。
//
// 用法: node scripts/check-frontend-classes.mjs
import { readFileSync, readdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const jsDir = join(root, 'web', 'js');

// 有意不写样式的类: 只作为语义标记或测试钩子存在。
// 加进这个名单要写清理由 —— 否则它会变成"藏问题的抽屉"。
const ALLOW_NO_STYLE = new Set([
  'ok', 'mint', 'warn', 'neutral', // 只是状态语义, 由 .pill.xxx 组合选择器命中
  'live',
  'is-active',
  'readonly',
  'probe',
]);

function collectUsedClasses() {
  const used = new Map(); // class -> 出现它的文件
  for (const file of readdirSync(jsDir).filter((f) => f.endsWith('.js'))) {
    const src = readFileSync(join(jsDir, file), 'utf8');
    // 只取静态写法: class: 'a b c' / class: `a b ${x}`
    for (const m of src.matchAll(/class:\s*(['"`])([^'"`]*)\1/g)) {
      // 模板字符串里的 ${...} 不是类名, 先消掉。
      // 这里还有一个坑: 正则遇到内层反引号会在那里截断(形如
      // `a ${x.startsWith(`${y}/`) ? 'b' : ''}`), 于是残留一段没有闭合的
      // `${...`。所以最后再按 "${" 截断一次, 只保留它前面的部分。
      const raw = m[2].replace(/\$\{[^}]*\}/g, ' ').split('${')[0];
      for (const token of raw.split(/\s+/).filter(Boolean)) {
        // 只认"像类名"的记号: 全小写字母数字与连字符。
        // 否则会把 === / || / path.startsWith( 这类残留当成类名报出来,
        // 让检查器自己变成噪声源。
        if (!/^[a-z][a-z0-9-]*$/.test(token)) continue;
        if (!used.has(token)) used.set(token, new Set());
        used.get(token).add(file);
      }
    }
  }
  return used;
}

function collectStyledClasses() {
  const css = readFileSync(join(root, 'web', 'style.css'), 'utf8');
  // 去掉注释, 避免把注释里提到的类名当成"已定义"。
  const withoutComments = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const styled = new Set();
  for (const m of withoutComments.matchAll(/\.([A-Za-z][A-Za-z0-9_-]*)/g)) {
    styled.add(m[1]);
  }
  return styled;
}

const used = collectUsedClasses();
const styled = collectStyledClasses();

const missing = [];
for (const [cls, files] of [...used].sort()) {
  if (styled.has(cls) || ALLOW_NO_STYLE.has(cls)) continue;
  missing.push({ cls, files: [...files].join(', ') });
}

if (missing.length > 0) {
  console.error('以下 class 在 JS 里使用了, 但样式表里没有定义:');
  for (const m of missing) {
    console.error(`  .${m.cls}   (出现在 ${m.files})`);
  }
  console.error('\n修法二选一: 在 web/style.css 里补上它的样式, 或者改用已有的类名。');
  console.error('如果它确实不需要样式, 请加到脚本的 ALLOW_NO_STYLE 里并写明理由。');
  process.exit(1);
}

console.log(`class 检查通过: JS 用到 ${used.size} 个 class, 全部有样式定义。`);

/* ---------------- 层叠回归断言 ---------------- */
//
// 上面那个检查只能发现"类没定义", 发现不了"类定义了但被别的规则压住"。
// 真实的 bug 就属于后者: .self-card 是给深色面试间写的绝对定位小卡片
// (position:absolute; top:74px; right:26px; width:136px), 而候选人自视
// 画面现在放在网格里 —— 只覆盖 position 是不够的, top:74px 依然生效,
// 于是预览框掉下来盖住了操作按钮。
//
// 所以这里直接算出层叠结果, 并断言关键属性被覆盖成了预期值。
// 这比"再看一眼截图"更可靠: 它不依赖某次人工观察, 每次改样式都会被重新验证。

function parseRules(cssText) {
  const text = stripMediaBlocks(cssText.replace(/\/\*[\s\S]*?\*\//g, ''));
  const rules = [];
  let index = 0;
  for (const m of text.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selector = m[1].trim();
    const body = m[2];
    // 只关心普通类选择器; @media 里的规则也会被上面的正则捕获,
    // 它们的 selector 里会带上 "@media ..." 前缀, 这里按"该选择器文本里
    // 是否出现类名"来判断匹配, 因此媒体查询内的规则同样能被正确计入。
    if (selector.startsWith('@')) continue;
    const declarations = {};
    for (const decl of body.split(';')) {
      const i = decl.indexOf(':');
      if (i < 0) continue;
      declarations[decl.slice(0, i).trim()] = decl.slice(i + 1).trim();
    }
    rules.push({ selector, declarations, order: index++ });
  }
  return rules;
}

// stripMediaBlocks 去掉 @media 块。
//
// 不这么做的话, 那批"只在窄屏生效"的规则会被当成始终生效 ——
// 例如 `@media (max-width: 820px) { .self-card { width: auto } }` 会让
// 检查器误以为 width 已经被覆盖, 从而放过桌面端的 `width: 136px`。
// 这里关心的是桌面层叠(出问题的就是它), 所以媒体查询内的规则整体忽略。
function stripMediaBlocks(cssText) {
  let out = '';
  let i = 0;
  while (i < cssText.length) {
    const at = cssText.indexOf('@media', i);
    if (at < 0) {
      out += cssText.slice(i);
      break;
    }
    out += cssText.slice(i, at);
    // 从 @media 后的第一个 '{' 开始做括号配对, 整块丢掉。
    let j = cssText.indexOf('{', at);
    if (j < 0) break;
    let depth = 0;
    for (; j < cssText.length; j += 1) {
      if (cssText[j] === '{') depth += 1;
      else if (cssText[j] === '}') {
        depth -= 1;
        if (depth === 0) break;
      }
    }
    i = j + 1;
  }
  return out;
}

// effective 计算"当一个元素拥有这些 classes 时, 某个属性的实际取值"。
//
// 匹配规则: 选择器里出现的类**必须都是该元素拥有的类**(选择器类是元素类的子集),
// 而不是反过来。写反了会把 `.self-card { top: auto }` 判成"不匹配
// .self-card.video 元素", 于是检查器自己给出假警报 —— 而这正是它第一版的表现。
//
// 特异性用类选择器个数近似; 特异性高的胜出, 相同则后出现的胜出,
// 与浏览器对同一份样式表的判定一致。
function effective(rules, classes, property) {
  const owned = new Set(classes);
  let winner = null;
  for (const rule of rules) {
    const selectorClasses = (rule.selector.match(/\.[A-Za-z][A-Za-z0-9_-]*/g) || []).map((c) => c.slice(1));
    if (selectorClasses.length === 0) continue;
    if (!selectorClasses.every((c) => owned.has(c))) continue;
    if (!(property in rule.declarations)) continue;
    const specificity = selectorClasses.length;
    if (
      winner === null
      || specificity > winner.specificity
      || (specificity === winner.specificity && rule.order > winner.order)
    ) {
      winner = { value: rule.declarations[property], specificity, order: rule.order, selector: rule.selector };
    }
  }
  return winner;
}

const cssText = readFileSync(join(root, 'web', 'style.css'), 'utf8');
const rules = parseRules(cssText);

// 断言: 候选人自视画面的容器不能被旧结构的绝对定位带跑。
const expectations = [
  { classes: ['self-card', 'video'], property: 'position', allow: ['relative', 'static'],
    why: '自视画面要按网格排布, 不能绝对定位' },
  { classes: ['self-card', 'video'], property: 'top', allow: ['auto', '0'],
    why: '旧的 top:74px 会把预览框顶下去盖住按钮' },
  { classes: ['self-card', 'video'], property: 'right', allow: ['auto', '0'],
    why: '旧的 right:26px 会把预览框从它的网格列里挪出来' },
  { classes: ['self-card', 'video'], property: 'width', allow: ['auto', '100%'],
    why: '旧的 width:136px 会让预览框只剩一小条' },
  { classes: ['room-question'], property: 'border-top', allow: null, nonEmpty: true,
    why: '浅色卡片里的分隔线必须重新指定颜色' },
];

const failures = [];
for (const exp of expectations) {
  const got = effective(rules, exp.classes, exp.property);
  const value = got ? got.value : null;
  if (value === null) {
    failures.push(`${exp.classes.map((c) => '.' + c).join('')} 的 ${exp.property} 没有任何规则覆盖 (${exp.why})`);
    continue;
  }
  if (exp.nonEmpty) continue;
  const normalized = value.toLowerCase().replace(/!important$/, '').trim();
  if (!exp.allow.includes(normalized)) {
    failures.push(`${exp.classes.map((c) => '.' + c).join('')} 的 ${exp.property} = ${value} (期望 ${exp.allow.join('/')}); ${exp.why}`);
  }
}

// 白字在浅色卡片上等于看不见 —— 这是同一类"旧结构配色泄漏"的问题。
const questionColor = effective(rules, ['room-question', 'question'], 'color');
if (!questionColor || /^#fff|^white|255,\s*255,\s*255/.test(questionColor.value.toLowerCase())) {
  failures.push(`.room-question .question 的 color 仍可能是白色 (${questionColor ? questionColor.value : '未定义'}); 面试问题会看不见`);
}

if (failures.length > 0) {
  console.error('\n样式层叠回归检查未通过:');
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}

console.log('样式层叠检查通过: 旧结构留下的定位与配色已被覆盖。');
