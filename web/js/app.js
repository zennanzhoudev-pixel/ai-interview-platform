// 应用入口: 路由、外壳导航、服务状态。
//
// 前端没有构建步骤, 因此这里是唯一的"装机点": 它决定当前是候选人空间、
// 招聘工作台还是管理台, 并把对应视图渲染到 #main。
import { el, clear, createRouter, navigate, store, api, toast, $ } from './core.js';
import {
  adoptLink, hasCandidateSession, signOutCandidate, renderOverview, renderPrep,
  renderRoom, renderCandidateReport,
} from './candidate.js';
import {
  ensureConsoleKey, renderKeyGate, renderDashboard, renderJobs, renderCandidates,
  renderPipeline, renderQuestions, renderSchedules, renderReports, renderReportDetail,
  renderSystem, disconnectConsole,
} from './console.js';
import { renderObserverRoom } from './observer.js';
import {
  loadMe, currentUser, logout, renderAuthPage, renderProfile, renderCandidateHistory,
} from './auth.js';

const main = document.getElementById('main');

/* ---------------- 服务状态 ---------------- */

// checkService 用健康检查判断服务是否可用。
//
// 之前的"服务未连接"提示会永久卡住: 它只在页面加载时请求一次, 失败后
// 再也不重试, 于是服务恢复了提示也不会消失 —— 看起来像功能坏了。
// 现在它会周期性重试, 并把失败原因写清楚。
const serviceState = { ok: null, reason: '', timer: null };

async function checkService() {
  const pill = document.getElementById('serviceStatus');
  try {
    const resp = await fetch('/readyz', { cache: 'no-store' });
    if (resp.ok) {
      serviceState.ok = true;
      serviceState.reason = '';
      if (pill) pill.hidden = true;
      return true;
    }
    serviceState.ok = false;
    serviceState.reason = `服务未就绪(${resp.status}), 存储可能不可用`;
  } catch (err) {
    serviceState.ok = false;
    serviceState.reason = '无法连接服务, 请确认服务已启动';
  }
  if (pill) {
    pill.hidden = false;
    pill.lastChild.textContent = serviceState.reason;
  }
  return false;
}

function startServiceWatch() {
  checkService();
  clearInterval(serviceState.timer);
  // 30 秒一轮: 足够及时, 又不会把探针打爆。
  serviceState.timer = setInterval(checkService, 30000);
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) checkService();
  });
}

/* ---------------- 外壳导航 ---------------- */

function renderShell(path) {
  const inConsole = path.startsWith('/console');
  const inObserver = path.startsWith('/observer');
  const navLinks = document.getElementById('navLinks');
  clear(navLinks);

  if (inConsole || inObserver) {
    [
      ['/console/dashboard', '看板'],
      ['/console/jobs', '职位'],
      ['/console/candidates', '候选人'],
      ['/console/pipeline', '管道'],
      ['/console/questions', '题库'],
      ['/console/schedules', '面试安排'],
      ['/console/reports', '报告'],
      ['/console/system', '系统'],
    ].forEach(([target, label]) => {
      navLinks.append(el('button', {
        class: `nav-link ${path === target || path.startsWith(`${target}/`) ? 'is-active' : ''}`,
        text: label,
        onclick: () => navigate(target),
      }));
    });
  } else {
    [
      ['/candidate', '概览'],
      ['/candidate/prep', '面试准备'],
      ['/candidate/room', '面试间'],
      ['/candidate/report', '我的报告'],
    ].forEach(([target, label]) => {
      navLinks.append(el('button', {
        class: `nav-link ${path === target ? 'is-active' : ''}`,
        text: label,
        onclick: () => navigate(target),
      }));
    });
  }

  const shell = document.getElementById('shellSwitch');
  clear(shell);
  // 用真实身份渲染页头: 显示"我是谁", 并提供个人中心与退出登录。
  // 之前的页头是写死的"访客", 登录后也看不出身份 —— 那会让人怀疑
  // 自己到底有没有登录成功。
  const user = currentUser();
  if (user) {
    shell.append(
      el('button', {
        class: 'btn ghost small',
        text: inConsole || inObserver ? '切换候选人空间' : '切换招聘工作台',
        onclick: () => navigate(inConsole || inObserver ? '/candidate' : '/console/dashboard'),
      }),
      el('button', { class: 'nav-link', text: '个人中心', onclick: () => navigate('/profile') }),
      el('button', { class: 'nav-link', text: '退出登录', onclick: () => logout() }),
    );
  } else {
    shell.append(el('button', {
      class: 'btn primary small',
      text: '登录 / 注册',
      onclick: () => navigate('/login'),
    }));
  }

  const avatar = document.getElementById('navAvatar');
  const menuName = document.getElementById('menuName');
  const menuRole = document.getElementById('menuRole');
  const apiKey = store.get('apiKey', '');
  if (user) {
    const u = user.user || {};
    if (avatar) avatar.textContent = (u.name || '我').slice(0, 1);
    if (menuName) menuName.textContent = u.name || '我的账号';
    if (menuRole) {
      menuRole.textContent = `${user.staff ? '企业成员' : '候选人'} · ${u.email || u.phone || ''}`;
    }
  } else if (inConsole && apiKey) {
    if (avatar) avatar.textContent = '台';
    if (menuName) menuName.textContent = '工作台(API Key)';
    if (menuRole) menuRole.textContent = '已用企业密钥连接';
  } else {
    if (avatar) avatar.textContent = '访';
    if (menuName) menuName.textContent = '未登录';
    if (menuRole) menuRole.textContent = hasCandidateSession() ? '已用面试链接进入' : '请登录或使用面试链接';
  }
}

/* ---------------- 路由表 ---------------- */

const routes = [
  { path: '/', render: () => navigate(homePath()) },
  { path: '/about', render: () => renderAbout(main) },
  { path: '/login', render: () => renderAuthPage(main, 'login') },
  { path: '/register', render: () => renderAuthPage(main, 'register') },
  { path: '/face', render: () => renderAuthPage(main, 'face') },
  { path: '/profile', render: () => renderProfile(main) },

  { path: '/candidate', render: () => withCandidate(() => renderOverview(main)) },
  { path: '/candidate/prep', render: () => withCandidate(() => renderPrep(main)) },
  { path: '/candidate/room', render: () => withCandidate(() => renderRoom(main)) },
  { path: '/candidate/report', render: () => withCandidate(() => renderCandidateReport(main)) },
  { path: '/candidate/history', render: () => withCandidate(() => renderCandidateHistory(main)) },

  { path: '/console', render: () => (ensureConsoleKey() ? navigate('/console/dashboard') : renderKeyGate(main)) },
  { path: '/console/dashboard', render: () => withConsole(() => renderDashboard(main)) },
  { path: '/console/jobs', render: () => withConsole(() => renderJobs(main)) },
  { path: '/console/candidates', render: () => withConsole(() => renderCandidates(main)) },
  { path: '/console/pipeline', render: () => withConsole(() => renderPipeline(main)) },
  { path: '/console/questions', render: () => withConsole(() => renderQuestions(main)) },
  { path: '/console/schedules', render: (params, query) => withConsole(() => renderSchedules(main, query)) },
  { path: '/console/reports', render: () => withConsole(() => renderReports(main)) },
  { path: '/console/reports/:id', render: (params) => withConsole(() => renderReportDetail(main, params.id)) },
  { path: '/console/system', render: () => withConsole(() => renderSystem(main)) },

  { path: '/observer/:id', render: (params) => renderObserverRoom(main, params.id) },
];

// withConsole 保证只有企业成员能进入招聘工作台。
//
// 这是"登录后按角色分流"的后半段: 服务端负责"登录后去哪",
// 前端负责"手输地址也进不去"。两道都要有 —— 只靠服务端返回的话,
// 候选人把地址改成 /console/dashboard 仍然能看到界面框架。
function withConsole(render) {
  const user = currentUser();
  if (user) {
    if (!user.staff) {
      // 候选人访问管理界面: 明确告知并送回自己的空间, 而不是给一个空白页。
      toast('这是招聘工作台, 候选人账号无法进入', 'error');
      navigate('/candidate');
      return;
    }
    render();
    return;
  }
  // 没登录: 先走登录页。API Key 是机器身份, 保留为集成场景的后路。
  if (!ensureConsoleKey()) {
    if (store.get('useApiKey', false)) {
      renderKeyGate(main);
      return;
    }
    navigate('/login');
    return;
  }
  render();
}

// withCandidate 保证候选人空间需要身份: 账号, 或者一条面试链接。
//
// 面试链接是刻意保留的: 候选人常常不想注册就能参加面试, 这时
// "一次性会话令牌"才是对的凭证。两者都允许, 但两者都必须有。
function withCandidate(render) {
  if (currentUser() || hasCandidateSession()) {
    render();
    return;
  }
  navigate('/login');
}

// homePath 决定"打开站点默认去哪": 已登录按角色走, 否则先去登录页。
export function homePath() {
  const user = currentUser();
  if (user) return user.staff ? '/console/dashboard' : '/candidate';
  if (hasCandidateSession()) return '/candidate';
  return '/login';
}

function notFound(segments) {
  clear(main);
  main.append(el('article', { class: 'card narrow center' }, [
    el('p', { class: 'eyebrow', text: '404' }),
    el('h1', { class: 'display small', text: '找不到这个页面' }),
    el('p', { class: 'muted mono', text: `/#/${segments.join('/')}` }),
    el('div', { class: 'actions' }, [
      el('button', { class: 'btn primary', text: '回到首页', onclick: () => navigate('/about') }),
      el('button', { class: 'btn outline', text: '去招聘工作台', onclick: () => navigate('/console/dashboard') }),
    ]),
  ]));
}

/* ---------------- 产品介绍 ---------------- */

// renderAbout 是完整的产品介绍页。
//
// 之前它是一个弹层, 内容被压缩成几段话。这个项目要说服的是用人部门与
// 合规同事, 因此介绍需要能被完整阅读、能被打印、能被逐条追问。
function renderAbout(root) {
  clear(root);
  root.append(
    el('div', { class: 'view-head' }, [
      el('div', {}, [
        el('p', { class: 'eyebrow', text: '产品介绍' }),
        el('h1', { class: 'display', text: '一场面试, 从流程到证据都说得清。' }),
        el('p', {
          class: 'lede',
          text: '面向企业招聘的 AI 线上面试中台: 把 1 到 5 轮面试做成可编排的流程, '
            + '用多 Agent 承担提问、追问与评分, 让每个结论都能回到候选人的原话。',
        }),
      ]),
      el('div', { class: 'head-actions' }, [
        el('button', { class: 'btn primary', text: '进入招聘工作台', onclick: () => navigate('/console/dashboard') }),
        el('button', { class: 'btn outline', text: '我是候选人', onclick: () => navigate('/candidate') }),
      ]),
    ]),
  );

  const sections = [
    {
      title: '一、面试怎么跑',
      body: '面试由状态机驱动, 固定六个阶段, 每步的时长与考察项都有预算。'
        + '调度同时受两个约束: 时间预算(剩余不足 20% 时只追问高重要性考点)与覆盖度(确保每个考察项都被问到)。'
        + '单题追问不超过 2 层, 全场超时会硬收口并把未覆盖项写进报告。',
      list: ['开场: 建立上下文', '经历深挖: 围绕简历项目逐层追问',
        '技术基础: 语言、运行时、分布式', '场景设计: 开放设计题与权衡',
        '候选人反问: 候选人提问, 不计分', '收尾: 同步后续流程'],
    },
    {
      title: '二、AI 怎么决定"追问什么"',
      body: '不是让模型自由发挥, 而是检索参考答案后挑缺口。'
        + '每道题的参考答案被拆成要点进检索索引, 检索是混合的: BM25(术语精确命中) + 向量(语义泛化) + RRF 融合 + 精排。'
        + '回答里没覆盖、又被参考答案强调的要点才会成为追问目标; 如果简历里恰好写到这个要点, 追问会直接引用简历原话并定位到原文偏移。',
      list: ['检索链路可被直接查看: 工作台里有"检索可视化"入口, 输入一句话就能看到命中结果与分数',
        '题目、评分要点、检索语料来自同一份数据, 因此报告依据与实际提问永远对得上',
        '没有配置嵌入模型时退化为本地特征哈希, 链路可回归、可压测'],
    },
    {
      title: '三、评分为什么可信',
      body: '每一层都在防"看起来合理但无法核对"的分数。',
      list: ['五级 rubric: 等级到分数的换算规则写死在代码里, 不让模型决定',
        '证据绑定: 每个维度分必须挂候选人原话, 拿不出证据的分数直接作废',
        '证据反查: 模型引用的原话必须真的出现在回答里, 编造的会被丢弃',
        '双模型交叉: 分歧在容忍范围内取保守值, 超阈值由第三方仲裁',
        '降级可见: 大模型不可用时自动回落到规则评分, 并在报告里标注'],
    },
    {
      title: '四、1 到 5 轮的 AI 参与度',
      body: 'AI 参与度随轮次递减, 终面永远由人决定; 同时保留"AI 落选 → 人类复议"的申诉通道。',
      table: true,
    },
    {
      title: '五、视频与真人协作',
      body: '三面之后人类面试官往往需要真的进房间。系统用端到端 WebRTC 让面试官与候选人建立点对点视频: '
        + '媒体不经过服务端, 服务端只转发协商信令, 因此既没有额外的带宽成本, 也没有"服务端能看到画面"的隐私风险。'
        + '旁听票据由 API Key 换取, 只对一场面试、一个角色、两小时有效, 不会把企业密钥暴露在 URL 里。',
      list: ['候选人侧: 摄像头自视、麦克风电平、离屏与遮挡提示',
        '面试官侧: 旁听席实时看到 AI 提问与作答进度、随时接入视频通话',
        '录像: 浏览器分片上传, 服务端按保留期自动清理, 播放行为写入审计'],
    },
    {
      title: '六、语音链路',
      body: '语音不是"额外功能", 而是面试体验本身。浏览器把麦克风音频转成 16kHz 单声道 PCM16, '
        + '按 20ms 一帧上行; 服务端做 VAD 端点检测(能量+过零率、噪声底自适应)、流式识别与打断级联取消。'
        + '打断有两条独立路径: 服务端检测到插话会取消在途的语音合成; 前端检测到用户说话会立刻停播并上报已播进度。',
      list: ['首字延迟是单独监控的指标: 从候选人说完到 AI 开始响应的耗时',
        '识别结果分 partial 与 final: 只有 final 参与评分, 避免"候选人还在补充, 系统已经打完分"',
        '未配置厂商密钥时, 语音链路会明确拒绝而不是静默降级成文字面试'],
    },
    {
      title: '七、编程轮次与判题沙箱',
      body: '二面(编程与实战)在真实招聘里是"现场写代码 + 跑用例"。代码会在隔离沙箱里执行: '
        + '网络关闭、内存与进程数受限、超时按进程组整体终止、输出长度截断。'
        + '隐藏用例的期望输出不会回传给候选人 —— 否则一段"打印期望值"的代码就能全绿。',
      list: ['判题响应里带 isolated 字段: 未隔离执行会被如实标注并给出告警',
        '没有容器运行时则降级为本机执行, 并明确说明"不具备隔离能力, 仅限本地开发"'],
    },
    {
      title: '八、招聘流程与看板',
      body: '面试不是一个孤立的动作。职位、候选人档案、投递管道、题库、面试安排与报告在同一个数据模型里, '
        + '面试结论会自动回流到看板的对应轮次。',
      list: ['1 到 5 轮怎么跑由职位配置决定: 哪轮 AI 主导、哪轮需要真人、哪轮是编程轮',
        'AI 只记录事实, 不做自动淘汰: 阶段推进与录用决定都由人做出并署名',
        '联系方式在落库前脱敏; 候选人可以导出或删除自己的数据'],
    },
    {
      title: '九、技术底座',
      body: '',
      list: ['接入: Go 长连接网关, 单条 WebSocket 承载文本、音频与视频信令',
        '实时音频: VAD 端点检测、流式识别、打断级联取消',
        '编排: 轮次状态机 + 问题 DAG; 断线重连靠重放已落库问答, 可跨进程恢复',
        '存储: MySQL(业务主数据) / Redis(会话快照) / 内存(零依赖演示), 三套实现共用行为契约测试',
        '治理: 多租户 RBAC、审计日志落库、按来源限流、panic 兜底、结构化日志、Prometheus 指标、OTel 链路追踪'],
    },
    {
      title: '十、数据与边界',
      body: '这些不是免责声明, 而是产品边界: 系统的能力上限在哪里, 我们选择明确说出来。',
      list: ['开始面试前需要候选人明示同意; 由招聘方发起的面试, 必须由候选人本人在面试间确认',
        '明确告知"本轮由 AI 面试官主持", 不做伪装成真人的设计',
        '不做情绪识别与面相分析; 反作弊只产出风险事件供人判断, 不自动淘汰',
        '面试录像按保留期自动清理; 删除候选人数据时录像内容与元数据一并删除'],
    },
  ];

  const doc = el('div', { class: 'about-grid' });
  sections.forEach((section) => {
    doc.append(el('article', { class: 'card doc' }, [
      el('h2', { text: section.title }),
      section.body ? el('p', { text: section.body }) : null,
      section.table ? roundTable() : null,
      section.list ? el('ul', { class: 'doc-list' }, section.list.map((item) => el('li', { text: item }))) : null,
    ]));
  });
  root.append(doc);

  root.append(el('article', { class: 'card' }, [
    el('h2', { text: '这套系统里, 有哪几件事是刻意不做的' }),
    el('ul', { class: 'doc-list' }, [
      el('li', { text: '不做"AI 自动淘汰": 误判的代价是毁掉一个人的机会, 因此只产出建议与风险事件。' }),
      el('li', { text: '不做"AI 伪装真人": 候选人有权知道对面是模型。' }),
      el('li', { text: '不做情绪与面相分析: 它们没有可靠的科学依据, 却会被当成客观结论使用。' }),
      el('li', { text: '不做"把录像永久留着": 无期限保留本身就是风险, 保留期是产品默认值而不是可选项。' }),
    ]),
  ]));
}

function roundTable() {
  const rows = [
    ['1 面', '全自动面试官', '100%', '仅异常复核'],
    ['2 面', '监考 + 判题 + 追问', '90%', '代码抽检'],
    ['3 面', '主持 + 抬杠式追问', '60%', '人类终审'],
    ['4 面', 'Copilot 实时辅助', '30%', '人类主导'],
    ['5 面', '初筛 + 纪要', '20%', 'HR 主导'],
  ];
  return el('table', { class: 'doc-table' }, [
    el('thead', {}, [el('tr', {}, [
      el('th', { text: '轮次' }), el('th', { text: 'AI 角色' }),
      el('th', { text: '参与度' }), el('th', { text: '人类角色' }),
    ])]),
    el('tbody', {}, rows.map((r) => el('tr', {}, r.map((cell) => el('td', { text: cell }))))),
  ]);
}

/* ---------------- 头像菜单 ---------------- */

function wireAvatarMenu() {
  const button = document.getElementById('avatarBtn');
  const menu = document.getElementById('avatarMenu');
  if (!button || !menu) return;
  button.addEventListener('click', (event) => {
    event.stopPropagation();
    menu.hidden = !menu.hidden;
    button.setAttribute('aria-expanded', String(!menu.hidden));
  });
  document.addEventListener('click', () => {
    menu.hidden = true;
    button.setAttribute('aria-expanded', 'false');
  });
  menu.querySelectorAll('[data-action]').forEach((item) => {
    item.addEventListener('click', () => {
      const action = item.dataset.action;
      menu.hidden = true;
      switch (action) {
        case 'about':
          navigate('/about');
          break;
        case 'profile':
          navigate('/profile');
          break;
        case 'history':
          navigate('/candidate/history');
          break;
        case 'console':
          navigate('/console/dashboard');
          break;
        case 'candidate':
          navigate('/candidate');
          break;
        case 'signout':
          // 两种身份一起退: 账号会话(服务端可撤销) 与面试链接令牌(本地)。
          // 只退一种会留下"看起来退出了、其实还能进"的状态。
          signOutCandidate();
          disconnectConsole();
          logout();
          break;
        case 'login':
          navigate('/login');
          break;
        case 'redetect':
          checkService().then((ok) => toast(ok ? '服务正常' : serviceState.reason, ok ? 'info' : 'error'));
          break;
        default:
          break;
      }
    });
  });
}

/* ---------------- 启动 ---------------- */

function boot() {
  // 面试链接形如 /?s=<session>&t=<token>: 先把它落到本地状态, 再清掉地址栏,
  // 免得令牌长期留在浏览器历史里。
  const params = new URLSearchParams(location.search);
  if (params.get('s') || params.get('t')) {
    adoptLink({}, Object.fromEntries(params));
    const clean = `${location.pathname}${location.hash || '#/candidate'}`;
    history.replaceState(null, '', clean);
  }
  ensureConsoleKey();
  startServiceWatch();
  wireAvatarMenu();

  // 先确认登录状态再解析路由: 否则刷新页面时会先渲染出候选人空间,
  // 几百毫秒后才跳去登录页 —— 那一瞬间的闪烁会让人以为"没登录也能进"。
  loadMe().finally(() => {
    createRouter(routes, notFound);
    window.addEventListener('hashchange', () => renderShell(currentPath()));
    renderShell(currentPath());
  });
}

function currentPath() {
  return location.hash.replace(/^#/, '') || '/';
}

document.addEventListener('DOMContentLoaded', boot);
