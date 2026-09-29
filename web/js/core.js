// 前端公共层: DOM 工具、API 客户端、状态、路由。
//
// 为什么不用框架、不用构建工具: 面试页面必须"任何环境下都能打开"。
// 企业内网、候选人自己的老旧浏览器、被墙的 CDN —— 白屏就是事故。
// 一个 HTML + 若干原生 ES 模块 + 一个 CSS, 通过 go:embed 打进二进制,
// 部署只有一个文件, 且永远不会因为外网依赖而无法加载。

/* ---------------- DOM 工具 ---------------- */

// el 创建元素。children 里的字符串会被当成文本(不解析 HTML),
// 因为这里的输入常常是候选人原话与简历内容 —— 用 innerHTML 会直接把
// 候选人可控的内容变成 XSS 注入点。
export function el(tag, attrs = {}, children = []) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === null || value === undefined || value === false) continue;
    if (key === 'class') node.className = value;
    else if (key === 'text') node.textContent = value;
    else if (key === 'html') node.innerHTML = value; // 仅在内部构造静态片段时使用
    else if (key === 'dataset') Object.assign(node.dataset, value);
    else if (key === 'style' && typeof value === 'object') Object.assign(node.style, value);
    else if (key.startsWith('on') && typeof value === 'function') {
      node.addEventListener(key.slice(2).toLowerCase(), value);
    } else if (value === true) node.setAttribute(key, '');
    else node.setAttribute(key, value);
  }
  for (const child of [].concat(children)) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export function $(selector, root = document) {
  return root.querySelector(selector);
}

export function $$(selector, root = document) {
  return Array.from(root.querySelectorAll(selector));
}

/* ---------------- 格式化 ---------------- */

export function fmtDate(value) {
  if (!value) return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString('zh-CN', { hour12: false });
}

export function fmtDay(value) {
  if (!value) return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleDateString('zh-CN');
}

export function fmtDuration(seconds) {
  const total = Math.max(0, Math.round(seconds || 0));
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
}

export function fmtBytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

// 能力项 key -> 中文标签。与后端 orchestrator 的标签表保持一致,
// 但这里必须再有一份: 前端不能因为"后端多了一个 key"就显示英文标识。
const COMPETENCY_LABELS = {
  project_depth: '项目深度',
  tech_choice: '技术选型',
  language_core: '语言与运行时',
  distributed_system: '分布式与中间件',
  architecture: '系统设计',
  communication: '沟通与协作',
  coding: '编码能力',
};

export function competencyLabel(key) {
  return COMPETENCY_LABELS[key] || key || '未归类';
}

const STAGE_LABELS = {
  INIT: '准备',
  GREETING: '开场',
  RESUME_DEEP_DIVE: '经历深挖',
  TECH_FUNDAMENTAL: '技术基础',
  SCENARIO_DESIGN: '场景设计',
  CANDIDATE_QA: '候选人反问',
  WRAP_UP: '收尾',
  SCORING: '评分',
  DONE: '已结束',
};

export function stageLabel(stage) {
  return STAGE_LABELS[stage] || stage || '—';
}

const PIPELINE_LABELS = {
  screening: '简历初筛',
  ai_interview: 'AI 面试',
  human_interview: '人工面试',
  offer: '已发 offer',
  hired: '已入职',
  rejected: '已淘汰',
  withdrawn: '候选人退出',
};

export function pipelineLabel(stage) {
  return PIPELINE_LABELS[stage] || stage || '—';
}

export const PIPELINE_ORDER = [
  'screening',
  'ai_interview',
  'human_interview',
  'offer',
  'hired',
  'rejected',
];

const RECOMMENDATION_LABELS = {
  STRONG_HIRE: '强烈推荐',
  HIRE: '推荐',
  PASS_WITH_CONCERN: '有条件通过',
  NO_HIRE: '不推荐',
  NEEDS_HUMAN_REVIEW: '需人工复核',
};

export function recommendationLabel(value) {
  return RECOMMENDATION_LABELS[value] || value || '待评估';
}

export function levelClass(level) {
  // 后端的等级是 "L1 未接触" 这样的中文展示值, 因此这里既认枚举名,
  // 也认 L1..L5 前缀 —— 只认其中一种的话, 报告里的颜色会全部退化成灰色。
  if (typeof level === 'string') {
    const matched = /^L([1-5])/.exec(level);
    if (matched) {
      const n = Number(matched[1]);
      if (n >= 4) return 'ok';
      if (n === 3) return 'mint';
      if (n === 2) return 'warn';
      return 'neutral';
    }
  }
  switch (level) {
    case 'EXEMPLARY':
    case 'STRONG':
      return 'ok';
    case 'ADEQUATE':
      return 'mint';
    case 'WEAK':
      return 'warn';
    default:
      return 'neutral';
  }
}

/* ---------------- Toast 与确认弹层 ---------------- */

export function toast(message, kind = 'info') {
  const node = document.getElementById('toast');
  if (!node) return;
  node.textContent = message;
  node.dataset.kind = kind;
  node.hidden = false;
  clearTimeout(node._timer);
  node._timer = setTimeout(() => {
    node.hidden = true;
  }, kind === 'error' ? 6000 : 3200);
}

// confirmDialog 返回 Promise<boolean>。
// 不用 window.confirm: 它会阻塞整个事件循环, 而面试间里还挂着
// WebSocket 与录音链路 —— 阻塞期间心跳发不出去, 连接会被判超时。
export function confirmDialog({ title, body, confirmText = '确认', danger = false }) {
  return new Promise((resolve) => {
    const overlay = el('div', { class: 'modal' });
    const close = (value) => {
      overlay.remove();
      resolve(value);
    };
    overlay.append(
      el('div', { class: 'modal-scrim', onclick: () => close(false) }),
      el('div', { class: 'modal-card narrow', role: 'dialog', 'aria-modal': 'true' }, [
        el('div', { class: 'modal-body' }, [
          el('p', { class: 'eyebrow', text: danger ? '不可撤销' : '请确认' }),
          el('h2', { class: 'display small', text: title }),
          el('p', { class: 'muted', text: body }),
          el('div', { class: 'actions' }, [
            el('button', { class: `btn ${danger ? 'danger' : 'primary'}`, text: confirmText, onclick: () => close(true) }),
            el('button', { class: 'btn outline', text: '取消', onclick: () => close(false) }),
          ]),
        ]),
      ]),
    );
    document.body.append(overlay);
  });
}

/* ---------------- 本地状态 ---------------- */

const STORAGE_KEY = 'ai-interview-os';

function loadState() {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY) || '{}');
  } catch {
    return {};
  }
}

const state = loadState();

export const store = {
  get(key, fallback = null) {
    return key in state ? state[key] : fallback;
  },
  set(key, value) {
    state[key] = value;
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
    } catch {
      // 隐私模式下 localStorage 可能不可用。此时功能降级为"不记忆",
      // 而不是抛异常把整个页面搞崩。
    }
  },
  remove(key) {
    delete state[key];
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
    } catch {
      /* 同上 */
    }
  },
};

/* ---------------- API 客户端 ---------------- */

export class ApiError extends Error {
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

// api 是带鉴权的 JSON 客户端。
//
// 每个响应都带上状态码与后端的中文错误文案: 前端不应该自己编错误信息,
// 因为后端最清楚"为什么拒绝"(权限不足、跨租户、同意缺失)。
export const api = {
  key: null,

  headers(extra = {}) {
    const headers = { 'Content-Type': 'application/json', ...extra };
    if (this.key) headers.Authorization = `Bearer ${this.key}`;
    return headers;
  },

  async request(method, path, { body, raw = false, headers = {} } = {}) {
    const init = { method, headers: this.headers(headers) };
    if (body !== undefined && body !== null) {
      init.body = typeof body === 'string' ? body : JSON.stringify(body);
    }
    let resp;
    try {
      resp = await fetch(path, init);
    } catch (err) {
      throw new ApiError(0, '无法连接服务: 请确认服务已启动', null);
    }
    if (raw) return resp;
    const text = await resp.text();
    let data = null;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch {
        data = { raw: text };
      }
    }
    if (!resp.ok) {
      const message = (data && (data.error || data.message)) || `请求失败(${resp.status})`;
      throw new ApiError(resp.status, message, data);
    }
    return data;
  },

  get(path, options) {
    return this.request('GET', path, options);
  },
  post(path, body, options) {
    return this.request('POST', path, { ...options, body });
  },
  patch(path, body, options) {
    return this.request('PATCH', path, { ...options, body });
  },
  del(path, options) {
    return this.request('DELETE', path, options);
  },
};

// candidateApi 用会话令牌访问候选人自己的接口。
export function candidateApi(token) {
  const withToken = (path) => {
    const sep = path.includes('?') ? '&' : '?';
    return `${path}${sep}token=${encodeURIComponent(token)}`;
  };
  return {
    token,
    url: withToken,
    get(path) {
      return api.get(withToken(path));
    },
    post(path, body) {
      return api.post(withToken(path), body);
    },
  };
}

/* ---------------- 路由 ---------------- */

// router 是一个极简的 hash 路由。
//
// 用 hash 而不是 History API: 前端被 go:embed 从根路径提供,
// 刷新 /console/reports 这类深层路径需要服务端配合重写, 而多一个
// 重写规则就多一个"线上 404"的可能。hash 路由没有这个问题。
export function createRouter(routes, notFound) {
  // 先编译路由表(把 "/console/reports/:id" 切成片段), 再定义 resolve。
  // 之前这里把未编译的原始数组传给了匹配循环, 于是 route.segments 永远是
  // undefined —— 整站所有视图都渲染不出来, 而报错只在浏览器控制台里。
  const compiled = routes.map((route) => ({
    ...route,
    segments: route.path.split('/').filter(Boolean),
  }));

  const resolve = () => {
    const raw = location.hash.replace(/^#/, '') || '/';
    const [pathPart, queryPart] = raw.split('?');
    const segments = pathPart.split('/').filter(Boolean);
    const query = Object.fromEntries(new URLSearchParams(queryPart || ''));

    for (const route of compiled) {
      if (route.segments.length !== segments.length) continue;
      const params = {};
      const matched = route.segments.every((seg, i) => {
        if (seg.startsWith(':')) {
          params[seg.slice(1)] = decodeURIComponent(segments[i]);
          return true;
        }
        return seg === segments[i];
      });
      if (matched) {
        route.render(params, query);
        return;
      }
    }
    notFound(segments, query);
  };

  window.addEventListener('hashchange', resolve);
  resolve();
  return {
    go(path) {
      if (location.hash === `#${path}`) resolve();
      else location.hash = path;
    },
    replace(path) {
      const url = `${location.pathname}${location.search}#${path}`;
      history.replaceState(null, '', url);
      resolve();
    },
  };
}

export function navigate(path) {
  location.hash = path;
}
