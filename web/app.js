// AI Interview OS 前端。
//
// 刻意不引入框架与构建步骤: 这个页面的职责是"把引擎状态显示出来,
// 把候选人的回答送回去"。引入框架只会增加部署面和故障面,
// 而面试场景下页面白屏就是事故。

const STAGES = [
  ["GREETING", "开场"],
  ["RESUME_DEEP_DIVE", "经历深挖"],
  ["TECH_FUNDAMENTAL", "技术基础"],
  ["SCENARIO_DESIGN", "场景设计"],
  ["CANDIDATE_QA", "候选人反问"],
  ["WRAP_UP", "收尾"],
];

const PREP_STEPS = [
  ["overview", "面试概览"],
  ["resume", "简历"],
  ["device", "设备检查"],
  ["consent", "数据同意"],
  ["done", "准备完成"],
];

const $ = (id) => document.getElementById(id);

const state = {
  profile: { name: "陈雨", company: "示例科技", position: "高级后端工程师", interviewer: "林澈" },
  prep: {
    step: 0,
    resume: "",
    resumeSkipped: false,
    deviceChecked: false,
    micLevel: 0,
    consentRecording: true,
    consentScoring: true,
    overviewSeen: false,
  },
  round: 1,
  minutes: 45,
  session: null,
  report: null,
  ws: null,
  voiceActive: false,
  recognition: null,
  speaking: false,
  preview: false,
  stage: null,
  elapsedBefore: 0,
  startedAt: 0,
  timer: null,
  mic: null,
};

/* ---------------- 基础工具 ---------------- */

function escapeHTML(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function toast(message) {
  const el = $("toast");
  el.textContent = message;
  el.hidden = false;
  clearTimeout(el._t);
  el._t = setTimeout(() => { el.hidden = true; }, 3600);
}

// fetchJSON 统一处理请求失败的两类情形。
//
// 浏览器原生的 "Failed to fetch" 只说"没拿到响应", 不说是服务没起还是网络断了,
// 对使用者毫无帮助 —— 而这个提示往往出现在"点了开始面试"这种最要命的时刻。
// 这里把它翻译成能指导下一步动作的话, 并顺带刷新服务状态指示。
async function fetchJSON(url, options) {
  let res;
  try {
    res = await fetch(url, options);
  } catch (_) {
    setServiceUp(false);
    throw new Error("无法连接面试服务：服务可能已停止或网络中断。确认服务在运行后重试。");
  }
  setServiceUp(true);

  let data = {};
  try {
    data = await res.json();
  } catch (_) {
    data = {};
  }
  if (!res.ok) {
    throw new Error(data.error || `请求失败（HTTP ${res.status}）`);
  }
  return data;
}

let serviceTimer = null;

function setServiceUp(up) {
  const pill = $("serviceStatus");
  if (pill) pill.hidden = up;
  const item = $("menuService");
  if (item) item.textContent = up ? "已连接" : "未连接（点击重新检测）";
}

// checkService 探测后端是否可用。
//
// 必须周期性重探: 只在页面加载时探一次的话, 服务恢复后提示会一直挂在那儿,
// 变成一条"过期但看起来像实时"的误导信息 —— 用户看到的明明是服务正常的
// 页面, 却被告诉服务没连上。
async function checkService() {
  try {
    const res = await fetch("/api/v1/health", { cache: "no-store" });
    setServiceUp(res.ok);
    return res.ok;
  } catch (_) {
    setServiceUp(false);
    return false;
  }
}

function startServiceWatch() {
  checkService();
  clearInterval(serviceTimer);
  serviceTimer = setInterval(checkService, 10000);
  // 切回标签页或窗口重新获得焦点时再探一次, 避免最长 10 秒的过期窗口。
  window.addEventListener("focus", checkService);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) checkService();
  });
}

/* ---------------- 头像菜单与产品介绍 ---------------- */

function toggleAvatarMenu(force) {
  const menu = $("avatarMenu");
  const btn = $("avatarBtn");
  const open = force === undefined ? menu.hidden : force;
  menu.hidden = !open;
  btn.setAttribute("aria-expanded", String(open));
  if (open) {
    $("menuName").textContent = state.profile.name;
    $("menuRole").textContent = `${state.profile.position} · ${state.profile.company}`;
    checkService();
  }
}

function openProduct() {
  const modal = $("productModal");
  modal.hidden = false;
  document.body.style.overflow = "hidden";
  modal.querySelector(".modal-body").scrollTop = 0;
}

function closeProduct() {
  $("productModal").hidden = true;
  document.body.style.overflow = "";
}

function loadProfile() {
  try {
    const raw = localStorage.getItem("interviewos.profile");
    if (raw) Object.assign(state.profile, JSON.parse(raw));
  } catch (_) { /* 隐私模式下 localStorage 可能不可用, 忽略即可 */ }
}

function saveProfile() {
  try {
    localStorage.setItem("interviewos.profile", JSON.stringify(state.profile));
  } catch (_) { /* 同上 */ }
}

function initials(name) {
  const chars = Array.from(String(name || "候选人"));
  return chars.slice(0, 1).join("");
}

/* ---------------- 视图切换 ---------------- */

function showView(view) {
  for (const id of ["viewOverview", "viewPrep", "viewRoom", "viewReport", "viewProfile"]) {
    $(id).hidden = id !== "view" + view[0].toUpperCase() + view.slice(1);
  }
  document.querySelectorAll("#navLinks .nav-link").forEach((b) => {
    b.classList.toggle("is-active", b.dataset.view === view);
  });
  window.scrollTo({ top: 0, behavior: "smooth" });

  if (view === "profile") refreshProfile();
}

/* ---------------- 概览 ---------------- */

function greetingText() {
  const h = new Date().getHours();
  if (h < 6) return "夜深了。";
  if (h < 12) return "上午好。";
  if (h < 18) return "下午好。";
  return "晚上好。";
}

// prepChecklist 是"准备进度"的唯一数据来源。
//
// 之前这里有两套算法: 概览按"实际完成了哪几项"数数, 准备页显示"当前翻到第几步"。
// 两者量纲不同, 于是把 5 步一路点到底就会显示 5/5, 而概览还停在 3/5 —— 数字互相打架。
// 现在两个页面都只读这份清单, 并且步骤条的"已完成"也按完成状态染色, 不再按位置。
//
// 简历允许显式跳过: 强制填简历会把"我只是想先跑一遍"的人挡在门外,
// 而"跳过"本身也会留下记录, 不会伪装成已完成。
function prepChecklist() {
  const p = state.prep;
  const resumeDone = String(p.resume || "").trim().length > 0 || p.resumeSkipped;
  const items = [
    { key: "overview", label: "面试概览", done: !!p.overviewSeen },
    { key: "resume", label: "简历", done: resumeDone, skipped: p.resumeSkipped && !String(p.resume || "").trim() },
    { key: "device", label: "设备检查", done: !!p.deviceChecked },
    { key: "consent", label: "数据同意", done: !!p.consentRecording },
  ];
  items.push({ key: "done", label: "准备完成", done: items.every((it) => it.done) });
  return items;
}

function prepProgress() {
  const items = prepChecklist();
  return { items, done: items.filter((it) => it.done).length, total: items.length };
}

function firstPendingStep() {
  const idx = prepChecklist().findIndex((it) => !it.done);
  return idx === -1 ? 4 : idx;
}

function stepperHTML(items) {
  return items.map((it, i) => {
    const cls = i === state.prep.step ? "active" : it.done ? "done" : "";
    const mark = it.done && i !== state.prep.step ? "✓" : String(i + 1);
    return `<li class="${cls}"><span class="num">${mark}</span>${it.label}</li>`;
  }).join("");
}

// refreshPrepProgressUI 只刷新"进度"相关的部分。
// 不能在这里重跑 renderPrep: 渲染会触发设备枚举, 枚举完成又要刷新进度,
// 那就会变成渲染与枚举互相触发的死循环。
function refreshPrepProgressUI() {
  const { items, done, total } = prepProgress();
  const count = $("prepStepCount");
  if (count) count.textContent = `第 ${state.prep.step + 1} / 5 步 · 已完成 ${done} / ${total} 项`;
  const stepper = $("stepper");
  if (stepper) stepper.innerHTML = stepperHTML(items);
}

function renderOverview() {
  const p = state.profile;
  $("greeting").textContent = greetingText() + p.name + "。";
  const { items, done, total } = prepProgress();
  const allDone = done === total;
  $("overviewLede").textContent = allDone
    ? "准备工作已完成，可以进入面试房间。"
    : `准备工作已完成 ${done} / ${total}，还差一点。`;
  $("navAvatar").textContent = initials(p.name);
  $("ovAvatar").textContent = (p.company || "OS").slice(0, 2).toUpperCase();
  $("ovPosition").textContent = p.position;
  $("ovCompany").textContent = `${p.company} · ${roundLabel(state.round)}`;
  $("ovDuration").textContent = `${state.minutes} 分钟`;
  $("ovInterviewer").textContent = `${p.interviewer} · 专业、中性`;
  $("ovTime").textContent = new Date(Date.now() + 2 * 864e5)
    .toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });

  const pct = Math.round((done / total) * 100);
  $("ovPrepPct").textContent = pct + "%";
  $("ovPrepBar").style.width = pct + "%";
  const pending = items.filter((it) => !it.done && it.key !== "done").map((it) => it.label);
  $("ovPrepHint").textContent = allDone
    ? "全部就绪，可以开始。"
    : `还需完成${pending.join("、")}。`;
  $("btnContinuePrep").textContent = allDone ? "进入面试房间" : "继续准备";
}

function roundLabel(round) {
  return ["", "第 1 面 · 技术基础", "第 2 面 · 编码算法", "第 3 面 · 系统设计",
    "第 4 面 · 领域交叉", "第 5 面 · HR 价值观"][round] || `第 ${round} 面`;
}

/* ---------------- 面试准备 ---------------- */

function renderPrep() {
  const p = state.profile;
  $("prepContext").textContent = `${p.company} · ${p.position}`;
  const { items, done, total } = prepProgress();
  // 同时给出"翻到第几步"和"完成了几项": 前者是导航位置, 后者才是进度。
  // 只显示前者会让人以为翻到底就等于做完了 —— 这正是之前两个数字打架的根源。
  $("prepStepCount").textContent = `第 ${state.prep.step + 1} / 5 步 · 已完成 ${done} / ${total} 项`;

  $("stepper").innerHTML = stepperHTML(items);

  // 最后一步不是"走过场", 它要把清单摊开: 哪些已完成、哪些还欠着。
  const allDone = done === total;
  const checklistRows = items.map((it) => `
    <li>
      <span>${it.done ? "✓" : "○"} ${it.label}${it.skipped ? "（已跳过）" : ""}</span>
      <b style="color:${it.done ? "var(--accent)" : "var(--warn)"}">${it.done ? "已完成" : "待完成"}</b>
    </li>`).join("");
  const donePanel = `
    <span class="pill ${allDone ? "mint" : "warn"}">${allDone ? "准备完成" : `还差 ${total - done} 项`}</span>
    <h2 class="panel-title">${allDone ? "可以开始了。" : "还有几项没完成。"}</h2>
    <p class="muted">${allDone
      ? "面试过程中你可以随时看到当前阶段、已用时长与追问状态。每个评分结论都会附带你的原话作为证据。"
      : "补齐下面标记为「待完成」的项目就能进入面试房间。简历可以跳过，设备检查与数据同意是必须的。"}</p>
    <ul class="checklist">${checklistRows}</ul>
    <div class="checklist" style="margin-top:6px">
      <li><span>应聘岗位</span><b>${escapeHTML(p.position)}</b></li>
      <li><span>公司与面试官</span><b>${escapeHTML(p.company)} · ${escapeHTML(p.interviewer)}</b></li>
      <li><span>时长预算</span><b>${state.minutes} 分钟</b></li>
    </div>`;

  const panels = {
    overview: `
      <span class="pill mint">技术面试</span>
      <h2 class="panel-title">${escapeHTML(p.position)}</h2>
      <p class="muted">这场面试约 ${state.minutes} 分钟，由 AI 面试官 ${escapeHTML(p.interviewer)} 主持。
      面试由状态机驱动，按时间预算与考察项覆盖度调度，最多追问两层。</p>
      <dl class="facts" style="margin-top:22px">
        <div><dt>公司</dt><dd>${escapeHTML(p.company)}</dd></div>
        <div><dt>面试官</dt><dd>${escapeHTML(p.interviewer)} · 专业、中性</dd></div>
        <div><dt>预计时长</dt><dd>${state.minutes} 分钟</dd></div>
        <div><dt>阶段</dt><dd>开场、经历、技术、场景、收尾</dd></div>
      </dl>`,

    resume: `
      <h2 class="panel-title">简历</h2>
      <p class="muted">把最能代表你的项目写在这里，面试会围绕它展开追问。</p>
      <label style="margin-top:16px">简历要点
        <textarea id="prepResume" rows="6"
          placeholder="例：IM 对话平台 Redis 存储改造，用 ZSet 索引把查询 RT 降低 70%">${escapeHTML(state.prep.resume)}</textarea>
      </label>
      <p class="muted small" style="margin-top:10px">
        填写后会在开始面试时提交给服务端做结构化解析（抽取技能、项目、时间线，并记录原文位置）。
        当追问涉及的要点正好出现在你的简历里，面试官会直接引用简历原话。
      </p>
      <div class="actions" style="margin-top:4px">
        <button class="btn outline small" type="button" id="prepSkipResume">
          ${state.prep.resumeSkipped ? "已跳过（点击取消跳过）" : "跳过简历"}
        </button>
      </div>`,

    device: `
      <h2 class="panel-title">设备检查</h2>
      <p class="muted">先确认浏览器能访问麦克风。语音链路（端点检测、流式识别、打断）
      已在服务端实现并测试，本次面试仍以键盘作答。</p>
      <ul class="checklist" id="deviceList"></ul>
      <div class="level-meter"><i id="micBar"></i></div>
      <div class="actions">
        <button class="btn outline small" id="btnDetect" type="button">检测设备</button>
        <button class="btn outline small" id="btnMicTest" type="button">测试麦克风（会请求权限）</button>
      </div>
      <p class="muted small" id="deviceHint" style="margin-top:10px"></p>`,

    consent: `
      <h2 class="panel-title">数据同意</h2>
      <p class="muted">招聘场景下录音与评分属于个人信息处理，需要你明确同意后才会开始。</p>
      <div style="margin-top:18px">
        <label class="check"><input type="checkbox" id="ckRecord" ${state.prep.consentRecording ? "checked" : ""} />
          我同意本轮面试录音，用于面试评估与后续复核</label>
        <label class="check"><input type="checkbox" id="ckScore" ${state.prep.consentScoring ? "checked" : ""} />
          我同意系统对我的回答进行自动评分，并知悉本轮由 AI 面试官主持</label>
      </div>
      <p class="muted small" style="margin-top:8px">
        同意记录会带上时间与来源 IP 保存在服务端，你可以在「个人资料」里随时核对。
      </p>`,

    done: donePanel,
  };

  $("prepPanel").innerHTML = panels[PREP_STEPS[state.prep.step][0]] +
    `<div class="actions">
       <button class="btn outline" id="prepPrev" ${state.prep.step === 0 ? "disabled" : ""}>上一步</button>
       <button class="btn primary" id="prepNext">${
         state.prep.step !== 4 ? "继续"
           : allDone ? "进入面试房间"
           : `去完成：${items[firstPendingStep()].label}`
       }</button>
     </div>`;

  wirePrepPanel();
}

function wirePrepPanel() {
  const prev = $("prepPrev");
  const next = $("prepNext");
  if (prev) prev.onclick = () => { state.prep.step = Math.max(0, state.prep.step - 1); renderPrep(); };
  if (next) next.onclick = () => {
    if (state.prep.step === 4) {
      const pending = firstPendingStep();
      // 清单没走完就不进房间, 而是把人送到缺的那一步 ——
      // 之前这里只在"未同意录音"时兜底, 其余未完成项会被静默放过。
      if (pending !== 4) {
        state.prep.step = pending;
        renderPrep();
        toast(`还差「${prepChecklist()[pending].label}」，完成后即可开始`);
        return;
      }
      startInterview();
      return;
    }
    state.prep.step = Math.min(4, state.prep.step + 1);
    renderPrep();
  };

  const resume = $("prepResume");
  if (resume) resume.oninput = () => { state.prep.resume = resume.value; };
  const skipResume = $("prepSkipResume");
  if (skipResume) skipResume.onclick = () => {
    // 再点一次取消跳过: 误触之后不应该只能靠"其实填了内容"来解套。
    state.prep.resumeSkipped = !state.prep.resumeSkipped;
    renderPrep();
  };

  const ckRecord = $("ckRecord");
  if (ckRecord) ckRecord.onchange = () => { state.prep.consentRecording = ckRecord.checked; };
  const ckScore = $("ckScore");
  if (ckScore) ckScore.onchange = () => { state.prep.consentScoring = ckScore.checked; };

  const detect = $("btnDetect");
  if (detect) detect.onclick = detectDevices;
  const micTest = $("btnMicTest");
  if (micTest) micTest.onclick = testMicrophone;

  // 设备枚举不需要权限弹窗, 进这一步就直接跑, 别让人先点一次按钮才知道结果。
  // 需要授权的是"测试麦克风", 那个仍然必须由用户主动点击。
  if (PREP_STEPS[state.prep.step][0] === "device") detectDevices();
}

// detectDevices 只枚举设备, 不申请权限 —— 枚举在未授权时也能拿到设备数量,
// 而申请权限会弹窗。把"弹窗"留给候选人主动点击的测试按钮,
// 是这类流程里最基本的礼貌。
async function detectDevices() {
  const list = $("deviceList");
  const hint = $("deviceHint");
  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) {
    list.innerHTML = `<li><span>浏览器媒体能力</span><b style="color:var(--warn)">不支持</b></li>`;
    hint.textContent = "当前浏览器不支持媒体设备访问（可能是非 HTTPS 环境或隐私模式）。";
    return;
  }
  try {
    const devices = await navigator.mediaDevices.enumerateDevices();
    const kinds = {
      audioinput: devices.filter((d) => d.kind === "audioinput").length,
      audiooutput: devices.filter((d) => d.kind === "audiooutput").length,
      videoinput: devices.filter((d) => d.kind === "videoinput").length,
    };
    state.prep.deviceChecked = true;
    hint.textContent = "设备枚举完成。语音作答用浏览器自带的识别与合成（Chrome/Edge），无需额外密钥。";
    renderDeviceList(kinds, true);
    refreshPrepProgressUI();
  } catch (err) {
    hint.textContent = "设备枚举失败：" + (err.message || err);
  }
}

function renderDeviceList(kinds, ok) {
  const list = $("deviceList");
  if (!list) return;
  const row = (label, value, good) =>
    `<li><span>${label}</span><b style="color:${good ? "var(--accent)" : "var(--warn)"}">${value}</b></li>`;
  list.innerHTML =
    row("麦克风", kinds.audioinput > 0 ? `检测到 ${kinds.audioinput} 个` : "未检测到", kinds.audioinput > 0) +
    row("扬声器", kinds.audiooutput > 0 ? `检测到 ${kinds.audiooutput} 个` : "未检测到", kinds.audiooutput > 0) +
    row("摄像头", kinds.videoinput > 0 ? `检测到 ${kinds.videoinput} 个` : "未检测到（本场不需要）", true) +
    row("浏览器媒体能力", ok ? "可用" : "不可用", ok);
}

// testMicrophone 是唯一会触发权限弹窗的动作, 因此必须由用户点击触发。
async function testMicrophone() {
  const hint = $("deviceHint");
  const bar = $("micBar");
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    hint.textContent = "当前环境不支持麦克风访问。";
    return;
  }
  try {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    const ctx = new (window.AudioContext || window.webkitAudioContext)();
    const source = ctx.createMediaStreamSource(stream);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 1024;
    source.connect(analyser);

    const buf = new Float32Array(analyser.fftSize);
    const startedAt = Date.now();
    const tick = () => {
      analyser.getFloatTimeDomainData(buf);
      let sum = 0;
      for (let i = 0; i < buf.length; i++) sum += buf[i] * buf[i];
      const rms = Math.sqrt(sum / buf.length);
      if (bar) bar.style.width = Math.min(100, Math.round(rms * 320)) + "%";
      if (Date.now() - startedAt < 8000) {
        state.micRaf = requestAnimationFrame(tick);
      } else {
        stopMicTest(stream, ctx);
      }
    };
    state.mic = { stream, ctx };
    tick();
    hint.textContent = "正在采集 8 秒，请正常说话看电平变化…";
    state.prep.deviceChecked = true;
  } catch (err) {
    hint.textContent = "麦克风不可用：" + (err.message || err);
  }
}

function stopMicTest(stream, ctx) {
  cancelAnimationFrame(state.micRaf);
  stream.getTracks().forEach((t) => t.stop());
  if (ctx && ctx.close) ctx.close();
  state.mic = null;
  const hint = $("deviceHint");
  if (hint) hint.textContent = "测试结束，麦克风工作正常。";
}

/* ---------------- 面试房间 ---------------- */

function renderRoomChrome() {
  const p = state.profile;
  $("roomTitle").textContent = `${p.company} · ${p.position}`;
  $("roomInterviewer").textContent = p.interviewer;
  $("roomAskLabel").textContent = `AI 面试官 · ${p.interviewer}`;
  $("roomCandidate").textContent = p.name;
  $("roomAvatar").textContent = initials(p.interviewer);
}

function setRoomStatus(text, live) {
  const el = $("roomStatus");
  el.innerHTML = `<i></i>${escapeHTML(text)}`;
  el.classList.toggle("live", !!live);
}

function renderStage() {
  const idx = Math.max(0, STAGES.findIndex(([key]) => key === state.stage));
  $("roomStage").textContent = `${roundLabel(state.round)} · ${idx + 1} / ${STAGES.length}`;
}

function startTimer() {
  clearInterval(state.timer);
  const tick = () => {
    const elapsed = state.elapsedBefore + Math.floor((Date.now() - state.startedAt) / 1000);
    const mm = String(Math.floor(elapsed / 60)).padStart(2, "0");
    const ss = String(elapsed % 60).padStart(2, "0");
    $("roomTimer").textContent = `${mm}:${ss} / ${state.minutes}:00`;
  };
  tick();
  state.timer = setInterval(tick, 1000);
}

function addTurn({ who, text, mine, probe }) {
  const el = document.createElement("div");
  el.className = "turn" + (mine ? " me" : "") + (probe ? " probe" : "");
  el.innerHTML = `<span class="who">${escapeHTML(who)}</span><div class="text">${escapeHTML(text)}</div>`;
  $("transcript").appendChild(el);
  el.scrollIntoView({ behavior: "smooth", block: "end" });
  return el;
}

function attachScore(turnEl, msg) {
  if (!turnEl) return;
  const chip = document.createElement("div");
  chip.className = "score-chip";
  if (!msg.scored) {
    chip.innerHTML = `<span>本环节不计分</span>`;
  } else {
    const quotes = (msg.evidence || [])
      .map((e) => `<span class="quote">${escapeHTML(e.quote)}</span>`)
      .join("");
    chip.innerHTML =
      `<span>评分 <b>${escapeHTML(msg.level)}</b></span>` +
      `<span>置信度 ${Number(msg.confidence).toFixed(2)}</span>` +
      (msg.degraded ? `<span style="color:var(--warn)">已降级到规则评分</span>` : "") +
      quotes;
  }
  turnEl.appendChild(chip);
}

async function startInterview() {
  const p = state.profile;
  const btn = $("prepNext");
  if (btn) btn.disabled = true;

  try {
    const data = await fetchJSON("/api/v1/sessions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        round: state.round,
        minutes: state.minutes,
        candidate_id: "c_" + encodeURIComponent(p.name),
        candidate_name: p.name,
        company: p.company,
        position: p.position,
        interviewer_name: p.interviewer,
        // 简历随会话一起提交: 服务端会做结构化解析(带原文偏移),
        // 之后追问命中简历里的要点时会直接引用简历原话。
        resume_text: state.prep.resume || "",
        consent_recording: state.prep.consentRecording,
      }),
    });

    state.session = data;
    state.round = data.round;
    state.minutes = data.minutes;
    state.preview = false;
    $("transcript").innerHTML = "";
    renderRoomChrome();
    showView("room");
    openSocket(data.session_id);
  } catch (err) {
    toast(err.message);
    if (btn) btn.disabled = false;
  }
}

function openSocket(sessionId) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/ws/interview/${encodeURIComponent(sessionId)}`);
  state.ws = ws;
  setRoomStatus("正在建立连接", true);

  ws.onmessage = (ev) => handleMessage(JSON.parse(ev.data));
  ws.onclose = () => {
    if (!state.report) setRoomStatus("连接已断开", false);
    $("btnSend").disabled = false;
  };
  ws.onerror = () => setRoomStatus("连接异常", false);
  $("btnSend").disabled = false;
}

function handleMessage(msg) {
  switch (msg.type) {
    case "state":
      state.round = msg.round;
      state.minutes = msg.minutes;
      state.elapsedBefore = msg.elapsed_sec || 0;
      state.startedAt = Date.now();
      state.stage = msg.stage;
      renderRoomChrome();
      renderStage();
      startTimer();
      if (msg.resumed) toast(`已从断点恢复，之前完成 ${msg.turn_count} 轮`);
      break;

    case "question":
      state.stage = msg.stage;
      renderStage();
      setRoomStatus(msg.probe ? "正在追问" : "等待你的回答", true);
      $("roomQuestion").textContent = msg.text;
      $("roomTags").textContent = `第 ${msg.index} 轮 · ${msg.probe ? "追问" : "主问题"}`;
      addTurn({
        who: `AI 面试官 · ${state.profile.interviewer}`,
        text: msg.text,
        probe: !!msg.probe,
      });
      speakQuestion(msg.text);
      $("answer").focus();
      break;

    case "turn_result":
      setRoomStatus("正在评分", false);
      attachScore($("transcript").querySelector(".turn.me:last-of-type"), msg);
      break;

    case "report":
      state.report = msg.payload;
      setRoomStatus("面试结束", false);
      clearInterval(state.timer);
      renderReport(msg.payload);
      showView("report");
      break;

    case "error":
      toast(msg.message || "服务端返回错误");
      break;
  }
}

function submitAnswer(ev) {
  ev.preventDefault();
  const text = $("answer").value.trim();
  $("answer").value = "";
  submitText(text);
}

/* ---------------- 语音(浏览器原生识别/合成) ----------------
 * 零密钥可用: 识别用 webkitSpeechRecognition, 合成用 SpeechSynthesis。
 * 服务端另有一套"二进制音频 + VAD + 打断"的语音链路, 配了 ASR/TTS 密钥
 * 才会启用(见 internal/api/audio.go); 这里的是浏览器原生的无密钥路径。
 * 两者共享同一套面试协议, 前端把识别结果当普通文本答案提交。 */

function voiceSupported() {
  return typeof window !== "undefined" &&
    (window.SpeechRecognition || window.webkitSpeechRecognition);
}

function toggleVoice() {
  if (!voiceSupported()) {
    toast("当前浏览器不支持语音识别(请用 Chrome 或 Edge)");
    return;
  }
  if (state.voiceActive) {
    stopVoice();
    return;
  }
  startVoice();
}

function startVoice() {
  const R = window.SpeechRecognition || window.webkitSpeechRecognition;
  const rec = new R();
  rec.lang = "zh-CN";
  rec.continuous = false;
  rec.interimResults = true;

  rec.onresult = (e) => {
    let interim = "";
    let final = "";
    for (let i = e.resultIndex; i < e.results.length; i++) {
      const t = e.results[i][0].transcript;
      if (e.results[i].isFinal) final += t;
      else interim += t;
    }
    if (interim) {
      // 一旦检测到用户说话, 立刻打断 AI 播报 —— 这是前端的 barge-in。
      if (state.speaking) speechSynthesis.cancel();
      state.speaking = false;
      const el = $("liveTranscript");
      el.hidden = false;
      el.textContent = "识别中… " + interim;
    }
    if (final) {
      $("liveTranscript").textContent = "已识别: " + final;
      submitText(final);
      stopVoice();
    }
  };
  rec.onerror = () => {
    $("liveTranscript").hidden = true;
    stopVoice();
  };
  rec.onend = () => {
    if (state.voiceActive && !state.speaking) {
      try { rec.start(); } catch (_) { /* 忽略连续会话重启失败 */ }
    }
  };

  state.recognition = rec;
  state.voiceActive = true;
  $("btnVoice").textContent = "停止语音";
  $("btnVoice").classList.add("primary");
  $("btnVoice").classList.remove("outline");
  try { rec.start(); } catch (_) { /* 权限被拒时由 onerror 兜底 */ }
}

function stopVoice() {
  state.voiceActive = false;
  if (state.recognition) {
    try { state.recognition.stop(); } catch (_) { /* ignore */ }
    state.recognition = null;
  }
  $("btnVoice").textContent = "语音";
  $("btnVoice").classList.remove("primary");
  $("btnVoice").classList.add("outline");
}

function submitText(text) {
  const t = String(text || "").trim();
  if (!t || !state.ws || state.ws.readyState !== WebSocket.OPEN) return;
  addTurn({ who: "我", text: t, mine: true });
  state.ws.send(JSON.stringify({ type: "answer", text: t }));
}

function speakQuestion(text) {
  if (!state.voiceActive || !("speechSynthesis" in window)) return;
  speechSynthesis.cancel();
  const u = new SpeechSynthesisUtterance(text);
  u.lang = "zh-CN";
  const zh = speechSynthesis.getVoices().find((v) => v.lang && v.lang.startsWith("zh"));
  if (zh) u.voice = zh;
  u.onstart = () => { state.speaking = true; };
  u.onend = () => { state.speaking = false; };
  u.onerror = () => { state.speaking = false; };
  speechSynthesis.speak(u);
}

function previewRoom() {
  state.preview = true;
  state.stage = "TECH_FUNDAMENTAL";
  state.minutes = state.minutes || 45;
  $("transcript").innerHTML = "";
  renderRoomChrome();
  renderStage();
  setRoomStatus("预览模式", false);
  $("roomQuestion").textContent = "请讲讲你如何权衡缓存一致性与服务可用性。";
  $("roomTags").textContent = "第 3 轮 · 主问题 · 这是预览，不会开始真实面试";
  $("roomTimer").textContent = "00:00 / 45:00";
  addTurn({
    who: `AI 面试官 · ${state.profile.interviewer}`,
    text: "请讲讲你如何权衡缓存一致性与服务可用性。",
  });
  showView("room");
}

/* ---------------- 报告 ---------------- */

// 报告里用到的展示映射。能力项在库里存英文 key, 展示用中文标签。
const COMPETENCY_LABELS = {
  project_depth: "项目深度",
  tech_choice: "技术选型",
  language_core: "语言与运行时",
  distributed_system: "分布式与中间件",
  architecture: "系统设计",
};

function compLabel(key) {
  return COMPETENCY_LABELS[key] || key || "—";
}

function stageLabel(key) {
  const found = STAGES.find(([k]) => k === key);
  return found ? found[1] : (key || "—");
}

function fmtDur(ms) {
  const sec = Math.round((ms || 0) / 1000);
  if (sec < 60) return sec + " 秒";
  return `${Math.floor(sec / 60)} 分 ${String(sec % 60).padStart(2, "0")} 秒`;
}

function recLabel(rec) {
  return {
    STRONG_HIRE: "强烈推荐",
    HIRE: "推荐通过",
    PASS_WITH_CONCERN: "通过但有顾虑",
    NO_HIRE: "不建议通过",
    UNDETERMINED: "证据不足",
  }[rec] || rec || "—";
}

// turnHTML 渲染一轮问答, 用于报告页的逐轮回放。
function turnHTML(t) {
  const final = (t.verdict && t.verdict.final) || {};
  const chip = t.scored
    ? `<span class="chip">${escapeHTML(final.level || "已评分")}</span>` +
      `<span class="chip">置信度 ${Number(final.confidence || 0).toFixed(2)}</span>`
    : `<span class="chip muted-chip">本环节不计分</span>`;
  const evidence = (final.evidence || [])
    .map((e) => `<li><span class="quote">${escapeHTML(e.quote)}</span></li>`)
    .join("");
  return `
    <li class="tl-item">
      <div class="tl-head">
        <span class="tl-index">${t.index}</span>
        <span class="tl-stage">${escapeHTML(stageLabel(t.stage))}${t.is_probe ? " · 追问" : ""}</span>
        <span class="tl-dur">${fmtDur(t.duration_ms)}</span>
      </div>
      <p class="tl-q">${escapeHTML(t.question)}</p>
      <p class="tl-a">${escapeHTML(t.answer)}</p>
      <div class="tl-meta">${chip}</div>
      ${evidence ? `<ul class="evidence-list">${evidence}</ul>` : ""}
    </li>`;
}

function renderReport(rep) {
  const p = state.profile;
  $("repSession").textContent = state.session ? state.session.session_id : "本地报告";
  $("repPosition").textContent = p.position;
  $("repMeta").textContent = `${p.company} · ${new Date().toLocaleDateString("zh-CN")}`;
  $("repScore").textContent = rep.score != null ? rep.score : "—";
  $("repScoreNote").textContent =
    "综合评分（各考察项等权平均）。分数用于组织观察，不代表客观结论；" +
    "只有当每一项都有原话证据支撑时，这个数字才有意义。";

  const dims = rep.dimensions || [];
  $("repDimensions").innerHTML = dims.map((d) => `
    <div class="dim-row">
      <span class="name">${escapeHTML(d.label || d.competency)}</span>
      <span class="bar"><i style="width:${Math.min(100, d.score)}%"></i></span>
      <span class="val">${d.score}</span>
    </div>`).join("");

  const detail = dims.map((d) => {
    const evidence = (d.evidence || [])
      .map((e) => `<li><span class="quote">${escapeHTML(e.quote)}</span></li>`)
      .join("");
    const concerns = (d.concerns || []).length
      ? `<p class="concerns">待确认要点：${escapeHTML(d.concerns.join(" / "))}</p>` : "";
    return `
      <div class="explain-item">
        <div class="explain-head">
          <span class="name">${escapeHTML(d.label || d.competency)}</span>
          <span class="meta">${escapeHTML(d.level)} · ${d.score} 分 · 置信度 ${Number(d.confidence).toFixed(2)} · ${d.turns} 轮</span>
        </div>
        ${evidence ? `<ul class="evidence-list">${evidence}</ul>` : ""}
        ${concerns}
      </div>`;
  }).join("");

  const s = rep.stats || {};
  const turns = rep.turns || [];
  const gaps = rep.gaps || [];
  const flags = rep.flags || [];

  // 结论摘要: 用真实数据拼一句话, 而不是写死的模板。
  const strong = dims.filter((d) => d.level_num >= 4);
  const weak = dims.filter((d) => d.level_num <= 2);
  const planned = dims.length + gaps.length;
  const coverage = planned ? Math.round((dims.length / planned) * 100) : 0;
  const evidenceCount = dims.reduce((n, d) => n + (d.evidence || []).length, 0);
  const summary = [
    `${dims.length} 个考察项中 ${strong.length} 项达到「精通」及以上`,
    weak.length ? `偏弱的是 ${weak.map((d) => d.label || compLabel(d.competency)).join("、")}` : "没有明显短板",
    gaps.length ? `${gaps.map(compLabel).join("、")} 未覆盖` : "考察项全部覆盖",
  ].join("；");

  // 各阶段实际耗时, 来自每一轮问答的服务端计时。
  const stageTotals = new Map();
  for (const t of turns) {
    const key = t.stage || "OTHER";
    stageTotals.set(key, (stageTotals.get(key) || 0) + (t.duration_ms || 0));
  }
  const stageRows = Array.from(stageTotals.entries()).sort((a, b) => b[1] - a[1]);
  const maxStageMs = Math.max(1, ...stageRows.map((r) => r[1]));

  const flagsHTML = flags.map((f) => `<div class="flag">${escapeHTML(f)}</div>`).join("");
  const gapsHTML = gaps.length
    ? `<div class="flag">未覆盖考察项：${escapeHTML(gaps.map(compLabel).join(" / "))}</div>` : "";

  $("repDetail").innerHTML = `
    <div class="card">
      <div class="summary">
        <div>
          <div class="badge ${escapeHTML(rep.recommendation || "")}">${escapeHTML(recLabel(rep.recommendation))}</div>
          <p class="muted small" style="margin-top:6px">AI 建议结论 · 置信度 ${Number(rep.confidence || 0).toFixed(2)}</p>
        </div>
        <p class="summary-text">${escapeHTML(summary)}。</p>
      </div>
      <div class="stats-grid">
        <div>考察项覆盖<b>${coverage}%</b></div>
        <div>证据条数<b>${evidenceCount}</b></div>
        <div>问答轮数<b>${s.turns || 0}</b></div>
        <div>用时 / 预算<b>${rep.duration_sec || 0}s / ${rep.budget_sec || 0}s</b></div>
      </div>
      ${flagsHTML}${gapsHTML}
    </div>

    <div class="card">
      <h2>能力维度</h2>
      <p class="muted small">每个等级都绑定候选人原话，可逐条回溯。</p>
      ${detail || "<p class='muted small'>本场没有产生可评分的考察项。</p>"}
    </div>

    <div class="card">
      <h2>面试节奏</h2>
      <p class="muted small">各阶段实际耗时，来自每一轮问答的服务端计时。</p>
      <div class="stage-timing">
        ${stageRows.map(([stage, ms]) => `
          <div class="timing-row">
            <span class="name">${escapeHTML(stageLabel(stage))}</span>
            <span class="bar"><i style="width:${Math.round((ms / maxStageMs) * 100)}%"></i></span>
            <span class="val">${fmtDur(ms)}</span>
          </div>`).join("") || "<p class='muted small'>没有可统计的轮次。</p>"}
      </div>
      <div class="stats-grid" style="margin-top:18px">
        <div>计分轮数<b>${s.scored_turns || 0}</b></div>
        <div>追问轮数<b>${s.probes || 0}</b></div>
        <div>最大追问深度<b>${s.max_probe_depth || 0} / 2</b></div>
        <div>平均置信度<b>${Number(s.avg_confidence || 0).toFixed(2)}</b></div>
      </div>
    </div>

    <div class="card">
      <h2>评分口径与来源</h2>
      <p class="muted small">
        等级到分数的换算规则写死在代码里，不由模型决定；综合分是各考察项的等权平均。
        拿不出原话证据的判断不会进入这个表。
      </p>
      <table class="doc-table">
        <thead><tr><th>等级</th><th>含义</th><th>分数</th></tr></thead>
        <tbody>
          <tr><td>L1 未掌握</td><td>无法描述基本概念</td><td>40</td></tr>
          <tr><td>L2 了解</td><td>知道概念，讲不出落地方式</td><td>58</td></tr>
          <tr><td>L3 熟练</td><td>能设计并说明权衡</td><td>74</td></tr>
          <tr><td>L4 精通</td><td>能预判瓶颈，给出容量估算或降级方案</td><td>88</td></tr>
          <tr><td>L5 专家</td><td>有跨系统权衡经验，能提出额外方案</td><td>96</td></tr>
        </tbody>
      </table>
      <div class="stats-grid" style="margin-top:18px">
        <div>双模型分歧<b>${s.disagreements || 0} 条</b></div>
        <div>三方仲裁<b>${s.arbitrations || 0} 条</b></div>
        <div>转人工复核<b>${s.human_review_items || 0} 条</b></div>
        <div>降级评分<b>${s.degraded_scores || 0} 条</b></div>
      </div>
    </div>

    <div class="card">
      <h2>逐轮问答回放</h2>
      <p class="muted small">面试官复核用：这一轮问了什么、候选人怎么答的、结论如何。</p>
      <ol class="timeline">
        ${turns.map(turnHTML).join("") || "<p class='muted small'>没有问答记录。</p>"}
      </ol>
    </div>`;
}

async function loadLatestReport() {
  if (state.report) { renderReport(state.report); return; }
  try {
    const data = await fetchJSON("/api/v1/sessions?limit=1");
    const first = (data.sessions || [])[0];
    if (!first) { $("repDetail").innerHTML = "<p class='muted small'>还没有面试记录。</p>"; return; }
    let payload;
    try {
      payload = await fetchJSON(`/api/v1/sessions/${encodeURIComponent(first.session_id)}/report`);
    } catch (err) {
      $("repDetail").innerHTML = `<p class="muted small">${escapeHTML(err.message)}</p>`;
      return;
    }
    state.session = first;
    state.report = payload;
    renderReport(payload);
  } catch (err) {
    $("repDetail").innerHTML = `<p class="muted small">加载失败：${escapeHTML(err.message)}</p>`;
  }
}

/* ---------------- 个人资料 ---------------- */

function renderProfileForm() {
  $("pfName").value = state.profile.name;
  $("pfCompany").value = state.profile.company;
  $("pfPosition").value = state.profile.position;
  $("pfInterviewer").value = state.profile.interviewer;
}

async function refreshProfile() {
  renderProfileForm();

  try {
    const data = await fetchJSON("/api/v1/sessions?limit=8");
    const sessions = data.sessions || [];
    $("pfSessions").innerHTML = sessions.length
      ? sessions.map((s) => `
          <div style="display:flex;justify-content:space-between;gap:12px;padding:10px 0;border-top:1px solid var(--line-2)">
            <span>${escapeHTML(s.session_id)}</span>
            <span>${escapeHTML(s.position || "")} · ${escapeHTML(s.status)} · ${escapeHTML(s.recommendation || "—")}</span>
          </div>`).join("")
      : "还没有面试记录。";

    const target = sessions[0];
    if (!target) { $("pfConsents").textContent = "还没有面试记录。"; return; }
    const cdata = await fetchJSON(`/api/v1/sessions/${encodeURIComponent(target.session_id)}/consents`);
    const consents = cdata.consents || [];
    $("pfConsents").innerHTML = consents.length
      ? consents.map((c) => `
          <div>
            <span>${escapeHTML(c.Scope || c.scope)}</span>
            <span>${escapeHTML(new Date(c.AgreedAt || c.agreed_at).toLocaleString("zh-CN"))} · ${escapeHTML(c.IP || c.ip || "")}</span>
          </div>`).join("")
      : "没有授权记录。";
  } catch (err) {
    $("pfSessions").textContent = "加载失败：" + err.message;
  }
}

/* ---------------- 事件绑定 ---------------- */

function bind() {
  $("navHome").onclick = () => { renderOverview(); showView("overview"); };
  document.querySelectorAll("#navLinks .nav-link").forEach((btn) => {
    btn.onclick = () => {
      const view = btn.dataset.view;
      if (view === "prep") { renderPrep(); showView("prep"); return; }
      if (view === "report") { renderReport(state.report || { recommendation: "", dimensions: [], stats: {} }); showView("report"); loadLatestReport(); return; }
      if (view === "overview") renderOverview();
      showView(view);
    };
  });
  $("navAbout").onclick = openProduct;

  // 头像菜单: 点开时补齐身份信息并顺带重探一次服务状态。
  $("avatarBtn").addEventListener("click", (e) => {
    e.stopPropagation();
    toggleAvatarMenu();
  });
  document.querySelectorAll("#avatarMenu .menu-item").forEach((item) => {
    item.addEventListener("click", (e) => {
      e.stopPropagation();
      const action = item.dataset.action;
      toggleAvatarMenu(false);
      switch (action) {
        case "profile":
          showView("profile");
          break;
        case "product":
          openProduct();
          break;
        case "redetect":
          checkService().then((ok) => toast(ok ? "服务已连接" : "服务仍未连接：确认服务在运行"));
          break;
        case "switch":
          state.session = null;
          state.report = null;
          showView("profile");
          $("pfName").focus();
          toast("修改下面的资料即可切换候选人身份");
          break;
      }
    });
  });
  document.addEventListener("click", () => toggleAvatarMenu(false));

  // 产品介绍弹层
  $("productClose").onclick = closeProduct;
  $("productCloseBottom").onclick = closeProduct;
  $("productScrim").onclick = closeProduct;
  $("productCta").onclick = () => {
    closeProduct();
    renderOverview();
    showView("overview");
  };
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      closeProduct();
      toggleAvatarMenu(false);
    }
  });

  $("btnContinuePrep").onclick = () => {
    // 进入准备流程即视为"看过面试概览", 这样两个页面的进度从一开始就一致。
    state.prep.overviewSeen = true;
    if (prepProgress().done === prepProgress().total) { startInterview(); return; }
    state.prep.step = firstPendingStep();
    renderPrep();
    showView("prep");
  };
  $("btnPreviewRoom").onclick = previewRoom;
  $("prepBack").onclick = () => { renderOverview(); showView("overview"); };
  $("formAnswer").addEventListener("submit", submitAnswer);
  $("btnVoice").addEventListener("click", toggleVoice);
  $("answer").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) $("formAnswer").requestSubmit();
  });
  $("btnAnother").onclick = () => {
    state.session = null;
    state.report = null;
    state.prep.step = 0;
    renderOverview();
    showView("overview");
  };
  $("btnPrint").onclick = () => window.print();

  for (const [id, key] of [["pfName", "name"], ["pfCompany", "company"],
    ["pfPosition", "position"], ["pfInterviewer", "interviewer"]]) {
    $(id).oninput = () => { state.profile[key] = $(id).value; saveProfile(); renderOverview(); };
  }
}

loadProfile();
bind();
renderOverview();
startServiceWatch();
