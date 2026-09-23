package media

import "strings"

// EndReason 说明"这一轮回答为什么结束"。
//
// 把结束原因显式建模而不是只返回一个 bool, 是因为它要进埋点:
// 如果线上大量回答以 max_duration 结束, 说明候选人在被题目卡住,
// 题目可能有问题; 如果大量以 semantic 结束, 说明端点判定过于激进。
type EndReason string

const (
	// EndNone 表示本轮回答还没结束。
	EndNone EndReason = ""
	// EndSilence 表示静音超过阈值, 判定说完。
	EndSilence EndReason = "silence"
	// EndSemantic 表示语义上已经收尾(比静音更早, 让节奏更紧凑)。
	EndSemantic EndReason = "semantic"
	// EndMaxDuration 表示单轮时长超上限, 强制收口(面试官要能掌控节奏)。
	EndMaxDuration EndReason = "max_duration"
	// EndASRFinal 表示 ASR 明确给出终止结果。
	EndASRFinal EndReason = "asr_final"
)

// EndpointerConfig 是端点判定策略。
type EndpointerConfig struct {
	// MaxUtteranceMS 是单轮回答时长上限, 超过强制收口。
	MaxUtteranceMS int
	// MinUtteranceMS 以下视为无效语音(咳嗽、椅子声), 不计入回答。
	MinUtteranceMS int
}

// DefaultEndpointerConfig 返回默认策略: 单轮最长 3 分钟, 短于 300ms 的视为噪声。
func DefaultEndpointerConfig() EndpointerConfig {
	return EndpointerConfig{
		MaxUtteranceMS: 180000,
		MinUtteranceMS: 300,
	}
}

// completionCues 是"说完了"的口语线索。
//
// 它们不是语法规则, 而是真实面试里候选人收尾时最常出现的说法。
// 有这个判断的意义在于: 候选人说完之后常常还有 1 到 2 秒的沉默,
// 纯靠静音判定会让 AI 的回应慢一拍, 面试节奏变得拖沓。
var completionCues = []string{
	"就这些", "就这么多", "大概这样", "差不多", "就这样", "以上",
	"回答完毕", "说完了", "没有了", "暂时没有",
}

// Endpointer 把"静音判定"和"语义判定"合成一个端点结论。
//
// 三层判定缺一不可:
//  1. 语义层: 候选人说"就这些"、句末标点收尾 -> 提前结束, 节奏紧凑;
//  2. 能量层: 静音超过阈值 -> 兜底结束;
//  3. 兜底层: 单轮超时 -> 强制结束, 防止候选人一直说下去。
type Endpointer struct {
	cfg            EndpointerConfig
	vad            *VAD
	utteranceFrame int64
}

// NewEndpointer 构造端点判定器。
func NewEndpointer(vadCfg VADConfig, cfg EndpointerConfig) *Endpointer {
	if cfg.MaxUtteranceMS <= 0 {
		cfg = DefaultEndpointerConfig()
	}
	return &Endpointer{cfg: cfg, vad: NewVAD(vadCfg)}
}

// OnFrame 送入一帧音频, 返回端点事件与结束原因。
func (e *Endpointer) OnFrame(pcm []byte) (VADEvent, EndReason) {
	ev := e.vad.ProcessFrame(pcm)

	switch ev {
	case VADSpeechStart:
		e.utteranceFrame = 0
		return ev, EndNone
	case VADSpeechEnd:
		ms := int64(e.vad.LastUtteranceMS())
		e.utteranceFrame = 0
		if ms < int64(e.cfg.MinUtteranceMS) {
			// 太短: 判定为咳嗽/环境音, 不构成一轮回答。
			return ev, EndNone
		}
		return ev, EndSilence
	}

	if e.vad.Speaking() {
		e.utteranceFrame++
		if e.utteranceFrame*FrameMS >= int64(e.cfg.MaxUtteranceMS) {
			return VADSilence, EndMaxDuration
		}
	}
	return ev, EndNone
}

// OnText 根据识别文本判断语义是否收尾。
//
// 只在 partial 结果上做启发式, final 结果直接结束 —— 因为 ASR 给出
// final 意味着它自己也认为这一句说完了。
func (e *Endpointer) OnText(text string, final bool) EndReason {
	if final {
		return EndASRFinal
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return EndNone
	}
	for _, cue := range completionCues {
		if strings.HasSuffix(t, cue) {
			return EndSemantic
		}
	}
	// 句末标点 + 足够长度: 说明已经完整表达了一句以上。
	if runeLen(t) >= 12 && strings.ContainsRune(sentenceEnders, lastRune(t)) {
		return EndSemantic
	}
	return EndNone
}

// Speaking 返回 VAD 当前是否处于说话态。
func (e *Endpointer) Speaking() bool { return e.vad.Speaking() }

// VAD 返回底层端点检测器, 便于观测噪声底等指标。
func (e *Endpointer) VAD() *VAD { return e.vad }

func lastRune(s string) rune {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	return rs[len(rs)-1]
}
