// 浏览器侧的采集与播放链路。
//
// 这一层是整个项目里唯一"必须由浏览器完成"的部分: 麦克风、摄像头、
// 编码、P2P 协商都只在浏览器里有权限。服务端已经把语音链路(VAD、
// 流式识别、打断级联取消)实现并测试完了, 这里要做的是把声音**真的**
// 送上去, 并把 AI 的声音**真的**播出来 —— 而不是让候选人用键盘打字。

/* ---------------- 设备与采集 ---------------- */

// mediaErrorMessage 把浏览器抛的错误码翻译成候选人能看懂的话。
//
// "NotAllowedError" 对候选人毫无意义, 而"你拒绝了麦克风权限,
// 请在地址栏左侧重新允许"能让他自己解决 —— 面试现场的这类卡点
// 如果没有可操作的提示, 一场面试就直接废掉。
export function mediaErrorMessage(err, kind = '设备') {
  const name = err && err.name ? err.name : '';
  switch (name) {
    case 'NotAllowedError':
    case 'SecurityError':
      return `${kind}权限被拒绝。请在浏览器地址栏左侧的图标里允许${kind}访问, 然后重试。`;
    case 'NotFoundError':
    case 'DevicesNotFoundError':
      return `没有检测到可用的${kind}设备。请确认设备已连接。`;
    case 'NotReadableError':
    case 'TrackStartError':
      return `${kind}被其他程序占用(例如另一个会议软件), 请关闭后重试。`;
    case 'OverconstrainedError':
      return `当前${kind}不支持所需的参数, 已尝试降低画质。`;
    case 'AbortError':
      return `${kind}启动被中断, 请重试。`;
    default:
      return `${kind}启动失败: ${err && err.message ? err.message : '未知错误'}`;
  }
}

// listDevices 列出可用设备。
//
// 浏览器在拿到权限之前不会返回设备名(label 为空), 因此这里会先申请
// 一次最宽松的权限再枚举 —— 否则设备下拉框里全是"摄像头 1 / 2",
// 候选人根本分不清哪个是自己的外接摄像头。
export async function listDevices() {
  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) {
    return { supported: false, cameras: [], microphones: [] };
  }
  try {
    const probe = await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
    probe.getTracks().forEach((t) => t.stop());
  } catch {
    // 拿不到权限也继续枚举: 至少能显示设备数量与类型。
  }
  const devices = await navigator.mediaDevices.enumerateDevices();
  return {
    supported: true,
    cameras: devices.filter((d) => d.kind === 'videoinput'),
    microphones: devices.filter((d) => d.kind === 'audioinput'),
  };
}

// openMedia 打开摄像头与麦克风。
export async function openMedia({ video = true, audio = true, cameraId, micId } = {}) {
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    throw new Error('当前浏览器不支持音视频采集, 请使用 Chrome / Edge / Safari 的最新版本。');
  }
  const constraints = {
    audio: audio
      ? {
          deviceId: micId ? { exact: micId } : undefined,
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
        }
      : false,
    video: video
      ? {
          deviceId: cameraId ? { exact: cameraId } : undefined,
          width: { ideal: 1280 },
          height: { ideal: 720 },
          frameRate: { ideal: 25, max: 30 },
        }
      : false,
  };
  return navigator.mediaDevices.getUserMedia(constraints);
}

/* ---------------- 麦克风: 电平 + PCM16 上行 ---------------- */

// MicCapture 把麦克风音频转成 16kHz 单声道 PCM16, 按 20ms 一帧回调。
//
// 采样率与帧长必须与服务端一致(media.SampleRate / media.FrameMS):
// 不一致的后果不是"音质差一点", 而是 VAD 端点判定与识别时间轴全体错位。
// 因此这里不信任 AudioContext 的实际采样率, 而是显式做重采样。
export class MicCapture {
  constructor(stream, { onFrame, onLevel, sampleRate = 16000, frameMS = 20 } = {}) {
    this.stream = stream;
    this.onFrame = onFrame;
    this.onLevel = onLevel;
    this.targetRate = sampleRate;
    this.frameSamples = (sampleRate * frameMS) / 1000;
    this.buffer = new Float32Array(0);
    this.running = false;
    this.muted = false;
  }

  async start() {
    const Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) throw new Error('当前浏览器不支持 Web Audio, 无法上行语音');
    this.ctx = new Ctx();
    if (this.ctx.state === 'suspended') await this.ctx.resume();
    this.source = this.ctx.createMediaStreamSource(this.stream);
    this.analyser = this.ctx.createAnalyser();
    this.analyser.fftSize = 512;
    this.source.connect(this.analyser);

    // ScriptProcessor 已废弃但在所有浏览器上都能用; AudioWorklet 更现代
    // 但需要额外的模块文件与更强的 CSP 兼容性。这里选前者并把理由写下来:
    // 面试场景下 4096 帧的回调粒度足够(约 85ms), 而它能保证"打开就能用"。
    const processor = this.ctx.createScriptProcessor(4096, 1, 1);
    processor.onaudioprocess = (event) => {
      if (!this.running || this.muted) return;
      const input = event.inputBuffer.getChannelData(0);
      const resampled = this.resample(input, this.ctx.sampleRate, this.targetRate);
      this.push(resampled);
    };
    this.processor = processor;
    this.source.connect(processor);
    // 必须接到 destination, 否则部分浏览器不会触发回调。
    const silent = this.ctx.createGain();
    silent.gain.value = 0;
    processor.connect(silent);
    silent.connect(this.ctx.destination);

    this.running = true;
    this.meterTimer = setInterval(() => this.reportLevel(), 80);
    return this;
  }

  // resample 做线性插值重采样。质量不如专业重采样器, 但对 16kHz
  // 语音识别完全够用, 且不需要引入任何依赖。
  resample(input, fromRate, toRate) {
    if (fromRate === toRate) return input;
    const ratio = fromRate / toRate;
    const outLength = Math.floor(input.length / ratio);
    const out = new Float32Array(outLength);
    for (let i = 0; i < outLength; i += 1) {
      const pos = i * ratio;
      const left = Math.floor(pos);
      const right = Math.min(left + 1, input.length - 1);
      const frac = pos - left;
      out[i] = input[left] * (1 - frac) + input[right] * frac;
    }
    return out;
  }

  push(samples) {
    const merged = new Float32Array(this.buffer.length + samples.length);
    merged.set(this.buffer);
    merged.set(samples, this.buffer.length);
    this.buffer = merged;
    while (this.buffer.length >= this.frameSamples) {
      const frame = this.buffer.subarray(0, this.frameSamples);
      this.buffer = this.buffer.subarray(this.frameSamples);
      if (this.onFrame) this.onFrame(toPCM16(frame));
    }
  }

  reportLevel() {
    if (!this.analyser || !this.onLevel) return;
    const data = new Uint8Array(this.analyser.fftSize);
    this.analyser.getByteTimeDomainData(data);
    let sum = 0;
    for (let i = 0; i < data.length; i += 1) {
      const v = (data[i] - 128) / 128;
      sum += v * v;
    }
    this.onLevel(Math.min(1, Math.sqrt(sum / data.length) * 4));
  }

  setMuted(muted) {
    this.muted = muted;
    if (this.stream) {
      this.stream.getAudioTracks().forEach((t) => {
        t.enabled = !muted;
      });
    }
  }

  stop() {
    this.running = false;
    clearInterval(this.meterTimer);
    try {
      if (this.processor) this.processor.disconnect();
    } catch {
      /* 已经断开 */
    }
    try {
      if (this.source) this.source.disconnect();
    } catch {
      /* 已经断开 */
    }
    if (this.ctx) this.ctx.close().catch(() => {});
  }
}

// toPCM16 把 [-1,1] 的浮点样本转成小端 PCM16。
// 单独一个函数是因为这段代码有三个容易写错的点: 裁剪范围、
// 负数的取整方向(必须向下取整, 否则会产生系统性直流偏移)、小端字节序。
export function toPCM16(samples) {
  const out = new ArrayBuffer(samples.length * 2);
  const view = new DataView(out);
  for (let i = 0; i < samples.length; i += 1) {
    const clamped = Math.max(-1, Math.min(1, samples[i]));
    view.setInt16(i * 2, clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff, true);
  }
  return out;
}

/* ---------------- AI 声音播放 + 打断 ---------------- */

// PcmPlayer 播放服务端下发的 PCM16 音频。
//
// 它同时承担"打断"的客户端一半: 候选人开始说话时, 前端必须立刻停播,
// 并把"已播到多少毫秒"回报服务端 —— 否则系统会认为自己把整句话说完
// 了, 而实际上候选人只听到一半。这个偏差会直接污染"AI 说了什么"的记录。
export class PcmPlayer {
  constructor({ sampleRate = 16000 } = {}) {
    this.sampleRate = sampleRate;
    this.queue = [];
    this.scheduledUntil = 0;
    this.playedMS = 0;
    this.playing = false;
  }

  async ensureContext() {
    if (this.ctx) {
      if (this.ctx.state === 'suspended') await this.ctx.resume();
      return this.ctx;
    }
    const Ctx = window.AudioContext || window.webkitAudioContext;
    this.ctx = new Ctx();
    this.gain = this.ctx.createGain();
    this.gain.connect(this.ctx.destination);
    return this.ctx;
  }

  async play(arrayBuffer) {
    const ctx = await this.ensureContext();
    const samples = new Int16Array(arrayBuffer);
    if (samples.length === 0) return;
    const buffer = ctx.createBuffer(1, samples.length, this.sampleRate);
    const channel = buffer.getChannelData(0);
    for (let i = 0; i < samples.length; i += 1) channel[i] = samples[i] / 32768;

    const source = ctx.createBufferSource();
    source.buffer = buffer;
    source.connect(this.gain);
    const startAt = Math.max(ctx.currentTime + 0.02, this.scheduledUntil);
    source.start(startAt);
    this.scheduledUntil = startAt + buffer.duration;
    this.playing = true;
    const durationMS = buffer.duration * 1000;
    this.queue.push({ source, durationMS });
    source.onended = () => {
      this.playedMS += durationMS;
      this.queue = this.queue.filter((q) => q.source !== source);
      if (this.queue.length === 0) this.playing = false;
    };
  }

  // interrupt 立刻停播, 返回"已经播出去的毫秒数"。
  interrupt() {
    const played = Math.round(this.playedMS);
    this.queue.forEach(({ source }) => {
      try {
        source.stop();
      } catch {
        /* 已经停了 */
      }
    });
    this.queue = [];
    this.playing = false;
    this.scheduledUntil = this.ctx ? this.ctx.currentTime : 0;
    return played;
  }

  reset() {
    this.playedMS = 0;
  }

  setVolume(value) {
    if (this.gain) this.gain.gain.value = Math.max(0, Math.min(1, value));
  }

  close() {
    this.interrupt();
    if (this.ctx) this.ctx.close().catch(() => {});
  }
}

/* ---------------- 录制: 分片上传 ---------------- */

// InterviewRecorder 用 MediaRecorder 把面试过程录下来, 边录边分片上传。
//
// 为什么不等录完再传: 45 分钟的会议是几百 MB, 而候选人可能在最后一分钟
// 关掉页面。按时间片上传意味着最坏只丢最后一片, 而不是整场面试的录像。
export class InterviewRecorder {
  constructor(stream, candidateApi, { kind = 'video', timesliceMS = 4000 } = {}) {
    this.stream = stream;
    this.api = candidateApi;
    this.kind = kind;
    this.timesliceMS = timesliceMS;
    this.index = 0;
    this.pending = [];
    this.uploading = false;
    this.startedAt = 0;
    this.bytes = 0;
  }

  supported() {
    return typeof window.MediaRecorder !== 'undefined' && this.stream
      && this.stream.getTracks().length > 0;
  }

  start() {
    if (!this.supported()) return false;
    this.recorder = new MediaRecorder(this.stream, { mimeType: pickMimeType() });
    this.startedAt = Date.now();
    this.recorder.ondataavailable = (event) => {
      if (!event.data || event.data.size === 0) return;
      this.pending.push(event.data);
      this.flush();
    };
    this.recorder.start(this.timesliceMS);
    return true;
  }

  // flush 串行上传: 分片必须按序到达, 否则服务端合并出来的文件是坏的。
  async flush() {
    if (this.uploading) return;
    this.uploading = true;
    try {
      while (this.pending.length > 0) {
        const blob = this.pending.shift();
        const index = this.index;
        this.index += 1;
        const buf = await blob.arrayBuffer();
        this.bytes += buf.byteLength;
        await fetch(
          this.api.url(`/api/v1/candidate/recording/chunks?kind=${this.kind}&index=${index}`),
          {
            method: 'POST',
            headers: {
              'Content-Type': this.recorder.mimeType || 'video/webm',
              Authorization: `Bearer ${this.api.token}`,
            },
            body: buf,
          },
        );
      }
    } catch {
      // 上传失败只影响录像留存, 不能影响面试本身; 最后 finalize 时
      // 会因为分片不全而失败, 届时会在界面上明确提示"本场录像不完整"。
    } finally {
      this.uploading = false;
    }
  }

  async stop() {
    if (!this.recorder || this.recorder.state === 'inactive') return null;
    await new Promise((resolve) => {
      this.recorder.onstop = resolve;
      this.recorder.stop();
    });
    await this.flush();
    const durationMS = Date.now() - this.startedAt;
    try {
      return await this.api.post('/api/v1/candidate/recording/finalize', {
        kind: this.kind,
        chunks: this.index,
        duration_ms: durationMS,
      });
    } catch (err) {
      return { error: err.message };
    }
  }
}

function pickMimeType() {
  const candidates = [
    'video/webm;codecs=vp9,opus',
    'video/webm;codecs=vp8,opus',
    'video/webm',
    'audio/webm',
  ];
  if (typeof MediaRecorder === 'undefined' || !MediaRecorder.isTypeSupported) return undefined;
  return candidates.find((t) => MediaRecorder.isTypeSupported(t));
}

/* ---------------- WebRTC: 与人类面试官的点对点视频 ---------------- */

// PeerLink 封装 RTCPeerConnection + 通过既有 WebSocket 走信令。
//
// 媒体走 P2P、信令走面试 WebSocket, 因此不需要新增端口、连接或部署组件。
// 服务端只转发 SDP/ICE, 看不到画面 —— 这既是成本考虑, 也是隐私考虑。
export class PeerLink {
  constructor({ ws, iceServers, selfId, onRemoteStream, onState, onPeerGone }) {
    this.ws = ws;
    this.iceServers = iceServers || [];
    this.selfId = selfId;
    this.onRemoteStream = onRemoteStream;
    this.onState = onState;
    this.onPeerGone = onPeerGone;
    this.peers = new Map();
  }

  // connect 向某个对端发起连接(候选人主动邀请面试官)。
  async connect(targetId, stream) {
    const pc = this.createPeer(targetId, stream);
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    this.send('webrtc.offer', targetId, { sdp: pc.localDescription.sdp, type: pc.localDescription.type });
    return pc;
  }

  createPeer(targetId, stream) {
    const existing = this.peers.get(targetId);
    if (existing) return existing;
    const pc = new RTCPeerConnection({ iceServers: this.iceServers });
    if (stream) stream.getTracks().forEach((track) => pc.addTrack(track, stream));
    pc.onicecandidate = (event) => {
      if (event.candidate) this.send('webrtc.ice', targetId, event.candidate.toJSON());
    };
    pc.ontrack = (event) => {
      if (this.onRemoteStream) this.onRemoteStream(targetId, event.streams[0]);
    };
    pc.onconnectionstatechange = () => {
      if (this.onState) this.onState(targetId, pc.connectionState);
      if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
        this.drop(targetId);
      }
    };
    this.peers.set(targetId, pc);
    return pc;
  }

  async handleSignal(msg) {
    const { type, from_id: fromId, payload } = msg;
    if (type === 'webrtc.peer_gone') {
      this.drop(fromId);
      if (this.onPeerGone) this.onPeerGone(fromId);
      return;
    }
    if (!fromId || fromId === this.selfId) return;
    if (type === 'webrtc.offer') {
      const pc = this.createPeer(fromId, this.localStream);
      await pc.setRemoteDescription({ type: 'offer', sdp: payload.sdp });
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      this.send('webrtc.answer', fromId, { sdp: pc.localDescription.sdp, type: pc.localDescription.type });
      return;
    }
    const pc = this.peers.get(fromId);
    if (!pc) return;
    if (type === 'webrtc.answer') {
      await pc.setRemoteDescription({ type: 'answer', sdp: payload.sdp });
    } else if (type === 'webrtc.ice' && payload) {
      try {
        await pc.addIceCandidate(payload);
      } catch {
        // ICE 候选乱序到达是正常的, 单个失败不应中断整条链路。
      }
    }
  }

  attachLocalStream(stream) {
    this.localStream = stream;
    if (!stream) return;
    this.peers.forEach((pc) => {
      const senders = pc.getSenders();
      stream.getTracks().forEach((track) => {
        const existing = senders.find((s) => s.track && s.track.kind === track.kind);
        if (existing) existing.replaceTrack(track);
        else pc.addTrack(track, stream);
      });
    });
  }

  send(type, targetId, payload) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    this.ws.send(JSON.stringify({ type, target_id: targetId, payload }));
  }

  drop(targetId) {
    const pc = this.peers.get(targetId);
    if (!pc) return;
    try {
      pc.close();
    } catch {
      /* 已关闭 */
    }
    this.peers.delete(targetId);
  }

  close() {
    this.peers.forEach((pc) => {
      try {
        pc.close();
      } catch {
        /* 已关闭 */
      }
    });
    this.peers.clear();
  }
}

/* ---------------- 防作弊信号 ---------------- */

// Proctor 采集"值得人类注意"的事件, 不做任何自动判定。
//
// 产品边界: 反作弊只产出风险事件链, 由人判断它意味着什么。
// 因此这里连"人脸识别"都不用真正的模型 —— 用画面变化做在场提示,
// 需要判断时交给人。任何"AI 自动判定候选人作弊"的能力都不该存在:
// 误判的代价是毁掉一个人的机会。
export class Proctor {
  constructor({ report, enabled = true } = {}) {
    this.report = report;
    this.enabled = enabled;
    this.handlers = [];
  }

  start(videoEl, { analyzeFace = true } = {}) {
    if (!this.enabled) return;
    const onVisibility = () => {
      if (document.hidden) this.signals.tabHidden += 1;
      this.emit(document.hidden ? 'tab_hidden' : 'tab_visible', `累计离开 ${this.signals.tabHidden} 次`);
    };
    const onBlur = () => this.emit('window_blur', '窗口失去焦点');
    const onContext = () => this.emit('context_menu', '打开了右键菜单');
    this.signals = { tabHidden: 0 };
    document.addEventListener('visibilitychange', onVisibility);
    window.addEventListener('blur', onBlur);
    document.addEventListener('contextmenu', onContext);
    this.handlers.push(
      () => document.removeEventListener('visibilitychange', onVisibility),
      () => window.removeEventListener('blur', onBlur),
      () => document.removeEventListener('contextmenu', onContext),
    );
    if (analyzeFace && videoEl) this.startPresenceProbe(videoEl);
  }

  // startPresenceProbe 用 1 秒一张的缩小灰度帧做"画面是否明显变化"的提示。
  // 它只在画面长时间完全静止(可能人离开了)或突然全黑(摄像头被遮挡)时
  // 产生一条提示, 不做身份判断。
  startPresenceProbe(videoEl) {
    const canvas = document.createElement('canvas');
    canvas.width = 32;
    canvas.height = 24;
    const ctx = canvas.getContext('2d');
    let last = null;
    let stillCount = 0;
    this.presenceTimer = setInterval(() => {
      if (videoEl.readyState < 2) return;
      ctx.drawImage(videoEl, 0, 0, canvas.width, canvas.height);
      const { data } = ctx.getImageData(0, 0, canvas.width, canvas.height);
      let avg = 0;
      for (let i = 0; i < data.length; i += 4) avg += data[i];
      avg /= data.length / 4;
      if (avg < 12) {
        this.emit('camera_blocked', '画面几乎全黑, 可能被遮挡');
        return;
      }
      if (last !== null && Math.abs(avg - last) < 0.5) {
        stillCount += 1;
        if (stillCount === 90) this.emit('frame_still', '画面持续 90 秒无变化');
      } else {
        stillCount = 0;
      }
      last = avg;
    }, 1000);
  }

  emit(type, detail) {
    if (this.report) this.report(type, detail);
  }

  stop() {
    clearInterval(this.presenceTimer);
    this.handlers.forEach((off) => off());
    this.handlers = [];
  }
}
