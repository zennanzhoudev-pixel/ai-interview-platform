package media

// VADEvent 是端点检测的输出事件。
type VADEvent int

const (
	// VADSilence 表示这一帧没有改变说话状态。
	VADSilence VADEvent = iota
	// VADSpeechStart 表示检测到人声开始。
	VADSpeechStart
	// VADSpeechEnd 表示检测到一段话结束。
	VADSpeechEnd
)

func (e VADEvent) String() string {
	switch e {
	case VADSpeechStart:
		return "speech_start"
	case VADSpeechEnd:
		return "speech_end"
	default:
		return "silence"
	}
}

// VADConfig 是端点检测参数。
//
// 全部做成可配不是过度设计: 不同企业客户的网络环境、麦克风质量、
// 办公区噪声差异极大, 一套写死的阈值必然有一批候选人被误判
// (要么 AI 一直抢话, 要么等十秒才回应)。
type VADConfig struct {
	// SpeechThreshold 是判定人声所需的能量倍数(相对自适应噪声底)。
	SpeechThreshold float64
	// StartFrames 是连续多少帧超阈值才判定说话开始, 用于过滤键盘声等瞬时噪声。
	// 它直接决定打断的响应速度, 所以取小值(60ms)。
	StartFrames int
	// EndFrames 是连续多少帧低于阈值才判定说完。
	// 这是整个 VAD 里最需要按业务调的参数: 太小会抢话, 太大会让候选人以为断了。
	EndFrames int
	// AbsFloor 是绝对能量下限。没有它, 在数字静音环境下噪声底会趋近 0,
	// 于是任何一点底噪都被判成人声。
	AbsFloor float64
	// NoiseFloorAlpha 是噪声底的自适应系数, 越小越平滑。
	NoiseFloorAlpha float64
}

// DefaultVADConfig 返回适合远程面试的一组默认值。
//
// 480ms 的静音判定比电话场景(通常 200ms)宽松得多 —— 因为面试里
// 候选人会停下来组织语言, 按电话节奏判定会把他的思考当成"说完了"。
func DefaultVADConfig() VADConfig {
	return VADConfig{
		SpeechThreshold: 3.0,
		StartFrames:     3,  // 60ms
		EndFrames:       24, // 480ms
		AbsFloor:        0.008,
		NoiseFloorAlpha: 0.02,
	}
}

// VAD 是基于能量与过零率的端点检测器。
//
// 能力边界要说清楚: 这是传统信号处理方案, 对稳定噪声的区分能力有限。
// 生产环境应该换成模型 VAD(Silero VAD 等), 接口不变。
// 保留这一实现的理由是它零依赖、可解释、可离线回归 —— 在 CI 里
// 能稳定复现, 而模型 VAD 需要额外权重文件和推理运行时。
type VAD struct {
	cfg VADConfig

	noiseFloor float64
	speaking   bool

	speechRun  int
	silenceRun int

	// voicedFrames 只统计"能量高于阈值"的帧。
	//
	// 用它而不是"说话态总帧数"来算语音时长: 说话态尾部必然包含最多
	// EndFrames 帧的静音, 把这段静音算进去会让 MinUtteranceMS 过滤
	// 完全失效 —— 一声咳嗽也能凑够 300ms。
	voicedFrames int64

	lastUtterMS int64
	totalFrames int64
	speechCount int64
}

// NewVAD 构造端点检测器。配置非法时回落到默认值。
func NewVAD(cfg VADConfig) *VAD {
	if cfg.SpeechThreshold <= 0 || cfg.StartFrames <= 0 || cfg.EndFrames <= 0 {
		cfg = DefaultVADConfig()
	}
	if cfg.AbsFloor <= 0 {
		cfg.AbsFloor = DefaultVADConfig().AbsFloor
	}
	if cfg.NoiseFloorAlpha <= 0 || cfg.NoiseFloorAlpha > 1 {
		cfg.NoiseFloorAlpha = DefaultVADConfig().NoiseFloorAlpha
	}
	return &VAD{cfg: cfg, noiseFloor: cfg.AbsFloor}
}

// ProcessFrame 送入一帧 PCM, 返回端点事件。
func (v *VAD) ProcessFrame(pcm []byte) VADEvent {
	energy := RMS(pcm)
	v.totalFrames++

	if !v.speaking {
		if energy > v.threshold() {
			v.speechRun++
			if v.speechRun >= v.cfg.StartFrames {
				v.speaking = true
				v.speechRun = 0
				v.silenceRun = 0
				// 触发说话开始的这几帧本身就是有声帧, 要计入时长。
				v.voicedFrames = int64(v.cfg.StartFrames)
				v.speechCount++
				return VADSpeechStart
			}
			return VADSilence
		}
		v.speechRun = 0
		// 噪声底只在非说话态更新。否则候选人一开口, 他的人声能量
		// 就会被当成"背景噪声"吃掉, 阈值被抬高, 导致后半句检测不到。
		v.noiseFloor = (1-v.cfg.NoiseFloorAlpha)*v.noiseFloor + v.cfg.NoiseFloorAlpha*energy
		return VADSilence
	}

	if energy > v.threshold() {
		v.voicedFrames++
		v.silenceRun = 0
		return VADSilence
	}

	v.silenceRun++
	if v.silenceRun >= v.cfg.EndFrames {
		v.speaking = false
		v.silenceRun = 0
		v.lastUtterMS = v.voicedFrames * FrameMS
		v.voicedFrames = 0
		return VADSpeechEnd
	}
	return VADSilence
}

func (v *VAD) threshold() float64 {
	t := v.noiseFloor * v.cfg.SpeechThreshold
	if t < v.cfg.AbsFloor {
		return v.cfg.AbsFloor
	}
	return t
}

// Speaking 返回当前是否处于说话态。
func (v *VAD) Speaking() bool { return v.speaking }

// LastUtteranceMS 返回上一段语音中"实际发声"的累计时长, 不含尾部静音。
// 端点判定用它来过滤咳嗽、敲键盘这类过短的噪声。
func (v *VAD) LastUtteranceMS() int64 { return v.lastUtterMS }

// NoiseFloor 返回当前估计的噪声底, 便于线上观测环境质量。
func (v *VAD) NoiseFloor() float64 { return v.noiseFloor }

// SpeechCount 返回检测到的语音段数。
func (v *VAD) SpeechCount() int64 { return v.speechCount }
