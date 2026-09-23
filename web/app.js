// 面试前端。刻意不引入任何框架与构建步骤:
// 这个页面的职责就是"把引擎的状态显示出来, 把候选人的回答送回去",
// 引入框架只会增加部署面和故障面。

const STAGES = [
  ["GREETING", "开场"],
  ["RESUME_DEEP_DIVE", "简历深挖"],
  ["TECH_FUNDAMENTAL", "技术基础"],
  ["SCENARIO_DESIGN", "场景设计"],
  ["CANDIDATE_QA", "候选人反问"],
  ["WRAP_UP", "收尾"],
];

const $ = (id) => document.getElementById(id);

const state = {
  sessionId: null,
  round: 1,
  minutes: 45,
  stage: null,
  elapsedBefore: 0,
  startedAt: 0,
  timer: null,
  ws: null,
  awaiting: false,
};

function show(view) {
  for (const v of ["viewSetup", "viewInterview", "viewReport"]) {
    $(v).hidden = v !== view;
  }
}

function toast(message) {
  const el = $("toast");
  el.textContent = message;
  el.hidden = false;
  clearTimeout(el._t);
  el._t = setTimeout(() => { el.hidden = true; }, 3200);
}

function renderRail(stage) {
  const rail = $("stageRail");
  const currentIndex = STAGES.findIndex(([key]) => key === stage);
  rail.innerHTML = STAGES
    .map(([key, label], i) => {
      let cls = "stage";
      if (currentIndex >= 0 && i < currentIndex) cls += " done";
      if (key === stage) cls += " current";
      return `<span class="${cls}">${label}</span>`;
    })
    .join("");
}

function startTimer() {
  clearInterval(state.timer);
  const budget = state.minutes * 60;
  const tick = () => {
    const elapsed = state.elapsedBefore + Math.floor((Date.now() - state.startedAt) / 1000);
    const mm = String(Math.floor(elapsed / 60)).padStart(2, "0");
    const ss = String(elapsed % 60).padStart(2, "0");
    $("timerText").textContent = `已用 ${mm}:${ss} / 预算 ${state.minutes}:00`;
    $("budgetText").textContent = `剩余 ${Math.max(0, Math.floor((budget - elapsed) / 60))} 分钟`;
  };
  tick();
  state.timer = setInterval(tick, 1000);
}

function bubble(role, who, text, extra) {
  const wrap = document.createElement("div");
  wrap.className = `msg ${role}`;
  const whoEl = document.createElement("div");
  whoEl.className = "who";
  whoEl.textContent = who;
  const body = document.createElement("div");
  body.className = "bubble";
  body.textContent = text;
  if (extra) {
    const extraEl = document.createElement("div");
    extraEl.className = "scored";
    extraEl.innerHTML = extra;
    body.appendChild(extraEl);
  }
  wrap.appendChild(whoEl);
  wrap.appendChild(body);
  $("chat").appendChild(wrap);
  wrap.scrollIntoView({ behavior: "smooth", block: "end" });
  return body;
}

function bindWS(sessionId) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/ws/interview/${encodeURIComponent(sessionId)}`);
  state.ws = ws;

  ws.onmessage = (event) => {
    const msg = JSON.parse(event.data);
    handleMessage(msg);
  };
  ws.onclose = () => {
    state.awaiting = false;
    $("btnSend").disabled = false;
  };
  ws.onerror = () => toast("连接异常, 请刷新页面重试");
}

function handleMessage(msg) {
  switch (msg.type) {
    case "state": {
      state.round = msg.round;
      state.minutes = msg.minutes;
      state.elapsedBefore = msg.elapsed_sec || 0;
      state.startedAt = Date.now();
      state.stage = msg.stage;
      renderRail(msg.stage);
      startTimer();
      $("topMeta").textContent = `第 ${msg.round} 面 · 会话 ${msg.session_id}` +
        (msg.resumed ? ` · 已从断点恢复（${msg.turn_count} 轮）` : "");
      if (msg.resumed) {
        toast(`已恢复上次进度: 已完成 ${msg.turn_count} 轮问答`);
      }
      break;
    }
    case "question": {
      state.stage = msg.stage;
      renderRail(msg.stage);
      bubble(msg.probe ? "probe" : "interviewer",
        msg.probe ? "追问" : "面试官",
        msg.text,
        `<span class="tag">第 ${msg.index} 轮 · ${msg.stage}</span>`);
      state.awaiting = false;
      $("btnSend").disabled = false;
      $("answer").focus();
      break;
    }
    case "turn_result": {
      const last = $("chat").lastElementChild;
      if (last && last.classList.contains("candidate")) {
        const extra = document.createElement("div");
        extra.className = "scored";
        if (msg.scored) {
          const evidence = (msg.evidence || [])
            .map((e) => `<span class="ev">${escapeHTML(e.quote)}</span>`)
            .join("");
          extra.innerHTML =
            `评分 <b>${escapeHTML(msg.level)}</b> · 置信度 ${msg.confidence.toFixed(2)}` +
            (msg.degraded ? ` · <span style="color:var(--warn)">已降级到规则评分</span>` : "") +
            evidence;
        } else {
          extra.innerHTML = `<span class="tag">本环节不计分</span>`;
        }
        last.querySelector(".bubble").appendChild(extra);
        last.scrollIntoView({ behavior: "smooth", block: "end" });
      }
      break;
    }
    case "report": {
      clearInterval(state.timer);
      renderReport(msg.payload);
      show("viewReport");
      break;
    }
    case "error": {
      toast(msg.message || "服务端返回错误");
      break;
    }
  }
}

function escapeHTML(s) {
  return String(s || "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function renderReport(rep) {
  const dims = (rep.dimensions || []).map((d) => {
    const evidence = (d.evidence || [])
      .map((e) => `<span class="ev">${escapeHTML(e.quote)}</span>`)
      .join("");
    const concerns = (d.concerns || []).length
      ? `<div class="concern">待确认要点: ${escapeHTML(d.concerns.join(" / "))}</div>` : "";
    return `
      <div class="dim">
        <div class="dim-head">
          <span class="dim-name">${escapeHTML(d.competency)}</span>
          <span class="dim-level">${escapeHTML(d.level)} · 置信度 ${Number(d.confidence).toFixed(2)} · ${d.turns} 轮</span>
        </div>
        <div class="bar"><i style="width:${(d.level_num / 5) * 100}%"></i></div>
        ${evidence}${concerns}
      </div>`;
  }).join("");

  const flags = (rep.flags || []).map((f) => `<div class="flag">⚠ ${escapeHTML(f)}</div>`).join("");
  const gaps = (rep.gaps || []).length
    ? `<div class="flag">未覆盖能力项: ${escapeHTML(rep.gaps.join(" / "))}</div>` : "";
  const s = rep.stats || {};

  $("report").innerHTML = `
    <div class="card">
      <div class="reco">
        <div>
          <div class="label">AI 建议结论</div>
          <div class="badge ${escapeHTML(rep.recommendation)}">${escapeHTML(rep.recommendation)}</div>
        </div>
        <div>
          <div class="label">整体置信度</div>
          <div class="badge" style="background:transparent;border:1px solid var(--line);color:var(--text)">${Number(rep.confidence).toFixed(2)}</div>
        </div>
        <div style="flex:1;min-width:180px">
          <div class="label">说明</div>
          <div style="font-size:13px;color:var(--muted)">这是给面试官的参考意见。真实系统里录用决策始终由人来做。</div>
        </div>
      </div>
      ${flags}${gaps}
    </div>

    <div class="card">
      <div class="kv">
        <div>问答轮数<b>${s.turns || 0}</b></div>
        <div>计分轮数<b>${s.scored_turns || 0}</b></div>
        <div>追问轮数<b>${s.probes || 0}</b></div>
        <div>最大追问深度<b>${s.max_probe_depth || 0}</b></div>
        <div>双模型分歧<b>${s.disagreements || 0}</b></div>
        <div>三方仲裁<b>${s.arbitrations || 0}</b></div>
        <div>转人工复核<b>${s.human_review_items || 0}</b></div>
        <div>降级评分<b>${s.degraded_scores || 0}</b></div>
        <div>耗时 / 预算<b>${rep.duration_sec || 0}s / ${rep.budget_sec || 0}s</b></div>
      </div>
    </div>

    <div class="card">
      <h1 style="font-size:17px;margin:0 0 4px">能力维度</h1>
      <p class="sub" style="margin-bottom:10px">每个等级都绑定候选人原话作为证据, 可逐条回溯。</p>
      ${dims || "<p class='sub'>本场没有产生可评分的考察项。</p>"}
    </div>`;
}

$("formSetup").addEventListener("submit", async (e) => {
  e.preventDefault();
  const btn = $("btnStart");
  btn.disabled = true;
  $("setupHint").textContent = "正在创建会话…";

  try {
    const res = await fetch("/api/v1/sessions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        round: Number($("round").value),
        minutes: Number($("minutes").value),
        candidate_id: $("candidate").value,
        consent_recording: $("consent").checked,
      }),
    });
    const data = await res.json();
    if (!res.ok) {
      throw new Error(data.error || `创建会话失败 (${res.status})`);
    }

    state.sessionId = data.session_id;
    state.round = data.round;
    state.minutes = data.minutes;
    state.elapsedBefore = 0;
    $("chat").innerHTML = "";
    show("viewInterview");
    bindWS(data.session_id);
    $("setupHint").textContent = "";
  } catch (err) {
    $("setupHint").textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});

$("formAnswer").addEventListener("submit", (e) => {
  e.preventDefault();
  const text = $("answer").value.trim();
  if (!text || !state.ws || state.ws.readyState !== WebSocket.OPEN) return;

  // 候选人的回答先上屏, 服务端的评分结果回来后再补充到同一条气泡里。
  bubble("candidate", "我", text);
  state.ws.send(JSON.stringify({ type: "answer", text }));
  $("answer").value = "";
});

$("answer").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
    $("formAnswer").requestSubmit();
  }
});

$("btnHistory").addEventListener("click", async () => {
  const box = $("history");
  box.hidden = false;
  box.innerHTML = "<p class='sub'>加载中…</p>";
  try {
    const res = await fetch("/api/v1/sessions?limit=10");
    const data = await res.json();
    const items = (data.sessions || []);
    box.innerHTML = items.length
      ? items.map((s) =>
          `<div class="hist-item">
             <span>${escapeHTML(s.session_id)} · 第 ${s.round} 面 · ${escapeHTML(s.status)}</span>
             <span>${escapeHTML(s.recommendation || "—")}</span>
           </div>`).join("")
      : "<p class='sub'>还没有面试记录。</p>";
  } catch (err) {
    box.innerHTML = `<p class="sub">加载失败: ${escapeHTML(err.message)}</p>`;
  }
});

$("btnAgain").addEventListener("click", () => {
  state.sessionId = null;
  show("viewSetup");
});
