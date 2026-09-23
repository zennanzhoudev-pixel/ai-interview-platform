package media

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
)

// EventType 是实时链路推给前端的事件类型。
type EventType string

const (
	EvUserSpeechStart   EventType = "user_speech_start"
	EvUserSpeechEnd     EventType = "user_speech_end"
	EvPartialTranscript EventType = "partial_transcript"
	EvFinalTranscript   EventType = "final_transcript"
	EvAssistantText     EventType = "assistant_text"
	EvAssistantAudio    EventType = "assistant_audio"
	EvAssistantSentence EventType = "assistant_sentence_done"
	EvInterrupted       EventType = "interrupted"
	EvTurnClosed        EventType = "turn_closed"
	EvError             EventType = "error"
)

// Event 是实时链路上的一条事件。
type Event struct {
	Type      EventType
	Text      string
	PCM       []byte
	Seq       int
	AtMS      int64
	Reason    EndReason
	HeardText string // 打断时: AI 实际被听到的内容
	Err       string
}

// Sink 接收链路事件。WebSocket 推送、测试断言都实现它。
type Sink interface {
	Emit(Event)
}

// SinkFunc 让普通函数适配 Sink。
type SinkFunc func(Event)

func (f SinkFunc) Emit(e Event) { f(e) }

// SessionConfig 配置实时链路行为。
type SessionConfig struct {
	VAD        VADConfig
	Endpointer EndpointerConfig
	ASR        ASRConfig
	Voice      Voice
	// InterruptGuardMS 是打断后强制静默时长。
	// 不留这个间隔, AI 会和候选人抢话, 听感上像在吵架。
	InterruptGuardMS int
	// EventBuffer 是事件通道容量。
	EventBuffer int
}

// DefaultSessionConfig 返回默认配置。
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		VAD:              DefaultVADConfig(),
		Endpointer:       DefaultEndpointerConfig(),
		ASR:              ASRConfig{Language: "zh", SampleRate: SampleRate},
		Voice:            Voice{ID: "default"},
		InterruptGuardMS: 200,
		EventBuffer:      256,
	}
}

// Session 管理一次面试的实时音频链路。
//
// 它把 VAD、ASR、TTS 三个异步流捏合成一个可控的对话轮次, 并负责整条
// 链路里最容易做错、也最影响体验的部分: 打断。
//
// 并发模型: 一次播报一个 turn, 打断通过 context 级联取消实现,
// 不依赖任何共享标志位轮询 —— 只有 context 才能把取消穿透到
// 最底层的 HTTP 请求。
type Session struct {
	cfg        SessionConfig
	asr        ASRProvider
	tts        TTSProvider
	endpointer *Endpointer

	events   chan Event
	stop     chan struct{}
	pumpDone chan struct{}
	sink     Sink

	droppedAudio int64

	mu              sync.Mutex
	root            context.Context
	rootCancel      context.CancelFunc
	speaking        *speakTurn
	listening       *listenTurn
	interruptedText string
	guardUntilMS    int64
	elapsedMS       int64
	closed          bool
	stopOnce        sync.Once
}

// NewSession 构造实时链路会话。
func NewSession(root context.Context, asr ASRProvider, tts TTSProvider, cfg SessionConfig, sink Sink) *Session {
	def := DefaultSessionConfig()
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = def.EventBuffer
	}
	if cfg.InterruptGuardMS <= 0 {
		cfg.InterruptGuardMS = def.InterruptGuardMS
	}
	ctx, cancel := context.WithCancel(root)
	s := &Session{
		cfg:        cfg,
		asr:        asr,
		tts:        tts,
		endpointer: NewEndpointer(cfg.VAD, cfg.Endpointer),
		events:     make(chan Event, cfg.EventBuffer),
		stop:       make(chan struct{}),
		pumpDone:   make(chan struct{}),
		sink:       sink,
		root:       ctx,
		rootCancel: cancel,
	}
	go s.pump()
	return s
}

// pump 把内部事件转给 Sink。
//
// 之所以要多一层 goroutine 而不是直接调用 Sink: 音频处理路径绝对不能被
// 下游(WebSocket 写、模型调用)阻塞。卡住的直接后果是候选人的回答被吞掉。
func (s *Session) pump() {
	defer close(s.pumpDone)
	for {
		select {
		case e := <-s.events:
			if s.sink != nil {
				s.sink.Emit(e)
			}
		case <-s.stop:
			return
		}
	}
}

// emit 推送事件。
//
// 音频分片允许丢弃(丢几帧只是听感上轻微断续), 但控制事件必须送达:
// 丢掉一条 final transcript 意味着候选人的一整段回答没有被记录,
// 直接导致评分偏差。所以两类事件用不同的背压策略。
func (s *Session) emit(e Event) {
	if e.Type == EvAssistantAudio {
		select {
		case s.events <- e:
		case <-s.stop:
		default:
			atomic.AddInt64(&s.droppedAudio, 1)
		}
		return
	}
	select {
	case s.events <- e:
	case <-s.stop:
	}
}

// Speak 合成并推送一段 AI 话术。
//
// 返回 context.Canceled 表示这一轮被打断 —— 这不是错误, 是正常业务路径。
// 调用方不应该把它记成失败告警, 否则线上会被假告警淹没。
func (s *Session) Speak(ctx context.Context, text string) error {
	sentences := SplitSentences(text)
	if len(sentences) == 0 {
		return nil
	}

	turnCtx, cancel := context.WithCancel(ctx)
	turn := &speakTurn{
		ctx:        turnCtx,
		cancel:     cancel,
		sentences:  sentences,
		sentenceMS: make([]int64, len(sentences)),
	}
	defer cancel()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errStreamClosed
	}
	s.speaking = turn
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.speaking == turn {
			s.speaking = nil
		}
		s.mu.Unlock()
		turn.closeStream()
	}()

	s.emit(Event{Type: EvAssistantText, Text: text, AtMS: s.elapsed()})

	for i, sentence := range sentences {
		if err := turnCtx.Err(); err != nil {
			return err
		}

		stream, err := s.tts.Speak(turnCtx, sentence, s.cfg.Voice)
		if err != nil {
			s.emit(Event{Type: EvError, Err: "TTS 合成失败: " + err.Error()})
			return err
		}
		turn.setStream(stream)

		for chunk := range stream.Chunks() {
			if turnCtx.Err() != nil {
				break
			}
			turn.addSentenceMS(i, PCMDurationMS(chunk.PCM))
			s.emit(Event{Type: EvAssistantAudio, PCM: chunk.PCM, Seq: chunk.Seq, AtMS: chunk.AtMS})
		}
		turn.setStream(nil)
		_ = stream.Close()

		if err := turnCtx.Err(); err != nil {
			return err
		}
		s.emit(Event{Type: EvAssistantSentence, Text: sentence, AtMS: s.elapsed()})
	}
	return nil
}

// OnAudioFrame 处理一帧候选人音频, 返回本帧是否触发了打断。
func (s *Session) OnAudioFrame(pcm []byte) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	now := s.elapsedMS
	s.elapsedMS += PCMDurationMS(pcm)
	inGuard := now < s.guardUntilMS
	s.mu.Unlock()

	if inGuard {
		// 打断后强制静默: 这段时间内完全不处理音频。
		//
		// 它防的是"AI 的输出还没播完, 又把自己唤醒"造成的抢话。
		// 代价是会丢掉候选人抢先说的头一两个字 —— 我们选择保住
		// 可听的连贯性, 因为抢话给人的感受比漏字糟糕得多。
		return false
	}

	ev, reason := s.endpointer.OnFrame(pcm)
	interrupted := false

	switch ev {
	case VADSpeechStart:
		if s.isSpeaking() {
			// 服务端自己检测到插话: 前端还没上报播放进度, 传 0 让它
			// 用"服务端已推送量"估算, 偏保守地认为 AI 说了更多。
			s.Interrupt(0)
			interrupted = true
		}
		s.startListening()
		s.emit(Event{Type: EvUserSpeechStart, AtMS: s.elapsed()})
	case VADSpeechEnd:
		s.emit(Event{Type: EvUserSpeechEnd, AtMS: s.elapsed(), Reason: reason})
		s.closeListening()
	}

	if reason != EndNone {
		s.emit(Event{Type: EvTurnClosed, Reason: reason, AtMS: s.elapsed()})
	}

	if lt := s.listeningTurn(); lt != nil {
		_ = lt.push(AudioChunk{PCM: pcm, AtMS: s.elapsed()})
	}
	return interrupted
}

// Interrupt 打断当前播报。
//
// playedMS 是前端上报的"已播放到第几毫秒"。传 0 表示前端未上报,
// 此时用服务端已推送量估算 —— 保守地认为 AI 说了更多,
// 因为重复比遗漏更伤体验。
func (s *Session) Interrupt(playedMS int64) {
	s.mu.Lock()
	turn := s.speaking
	s.mu.Unlock()
	if turn == nil {
		return
	}

	heard := turn.heardText(playedMS)

	// 先通知前端清空本地播放缓冲, 再取消服务端流。
	// 顺序反过来会出现: 服务端已经不产音频了, 客户端还在播缓冲里的旧内容,
	// 候选人会听到 AI 自顾自把话说完。
	s.emit(Event{Type: EvInterrupted, HeardText: heard, AtMS: s.elapsed()})

	turn.cancel()      // 级联取消: LLM 流式生成、TTS HTTP 请求都会被中断
	turn.closeStream() // 显式关闭底层流, 立刻释放连接

	s.mu.Lock()
	s.interruptedText = heard
	s.guardUntilMS = s.elapsedMS + int64(s.cfg.InterruptGuardMS)
	s.mu.Unlock()
}

// ConsumeInterrupted 取出并清空"上次被打断时 AI 已说出的内容"。
//
// 调用方(编排层)把它注入下一轮上下文, 让 AI 知道自己说到哪了。
// 这是语音面试体验的分水岭: 被打断后把同一句话从头再说一遍,
// 候选人会立刻感到"对面没在听我说话"。
func (s *Session) ConsumeInterrupted() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.interruptedText
	s.interruptedText = ""
	return t
}

// Close 关闭会话并释放所有流。
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	turn := s.speaking
	s.mu.Unlock()

	s.closeListening()
	if turn != nil {
		turn.cancel()
		turn.closeStream()
	}
	s.rootCancel()
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.pumpDone
}

// DroppedAudio 返回因背压被丢弃的音频分片数, 用于观测。
func (s *Session) DroppedAudio() int64 { return atomic.LoadInt64(&s.droppedAudio) }

// Endpointer 暴露端点判定器, 便于观测噪声底等质量指标。
func (s *Session) Endpointer() *Endpointer { return s.endpointer }

// ElapsedMS 返回链路已处理的音频时长。
func (s *Session) ElapsedMS() int64 { return s.elapsed() }

func (s *Session) isSpeaking() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.speaking != nil
}

func (s *Session) elapsed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.elapsedMS
}

func (s *Session) listeningTurn() *listenTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listening
}

func (s *Session) startListening() {
	s.mu.Lock()
	if s.listening != nil || s.closed {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.root)
	s.mu.Unlock()

	stream, err := s.asr.Open(ctx, s.cfg.ASR)
	if err != nil {
		cancel()
		s.emit(Event{Type: EvError, Err: "ASR 打开失败: " + err.Error()})
		return
	}

	lt := &listenTurn{stream: stream, cancel: cancel}
	s.mu.Lock()
	s.listening = lt
	s.mu.Unlock()

	go s.consumeASR(lt)
}

func (s *Session) closeListening() {
	s.mu.Lock()
	lt := s.listening
	s.listening = nil
	s.mu.Unlock()
	if lt != nil {
		_ = lt.close()
	}
}

func (s *Session) consumeASR(lt *listenTurn) {
	defer lt.cancel()
	for res := range lt.stream.Results() {
		typ := EvPartialTranscript
		if res.Final {
			typ = EvFinalTranscript
		}
		s.emit(Event{Type: typ, Text: res.Text, AtMS: res.AtMS})

		if res.Final {
			continue
		}
		if reason := s.endpointer.OnText(res.Text, false); reason != EndNone {
			// 语义已经收尾: 不必再等满 480ms 静音, 直接收口。
			// 这是"纯静音判定"与"语义加静音双层判定"的差别 ——
			// 后者让面试节奏明显更紧凑。
			s.emit(Event{Type: EvTurnClosed, Reason: reason, Text: res.Text, AtMS: res.AtMS})
			s.closeListening()
		}
	}
}

// speakTurn 表示一次 AI 播报。
type speakTurn struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	sentences  []string
	sentenceMS []int64
	sentMS     int64
	stream     TTSStream
}

func (t *speakTurn) addSentenceMS(i int, ms int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i >= 0 && i < len(t.sentenceMS) {
		t.sentenceMS[i] += ms
	}
	t.sentMS += ms
}

func (t *speakTurn) setStream(s TTSStream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stream = s
}

func (t *speakTurn) closeStream() {
	t.mu.Lock()
	s := t.stream
	t.stream = nil
	t.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// heardText 返回在 playedMS 处被打断时, AI 实际被听到的内容。
//
// 只统计"完整播完"的句子: 半句被截断的内容听感上是残缺的,
// 把它计入上下文会让 AI 以为自己说过完整的一句话, 反而制造新的错乱。
func (t *speakTurn) heardText(playedMS int64) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if playedMS <= 0 {
		playedMS = t.sentMS
	}
	var b strings.Builder
	var acc int64
	for i, s := range t.sentences {
		if acc+t.sentenceMS[i] > playedMS {
			break
		}
		b.WriteString(s)
		acc += t.sentenceMS[i]
	}
	return strings.TrimSpace(b.String())
}

// listenTurn 表示一次候选人回答的采集过程。
type listenTurn struct {
	stream ASRStream
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
}

func (l *listenTurn) push(c AudioChunk) error {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return errStreamClosed
	}
	return l.stream.Push(c)
}

func (l *listenTurn) close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	return l.stream.Close()
}
