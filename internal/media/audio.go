// Package media 实现面试的实时音频链路: VAD 端点检测、流式 ASR/TTS、打断(barge-in)。
//
// 这一层最容易做成"看起来能用"的空壳: 接口定义得漂漂亮亮, 但打断响应、
// 端点判定、级联取消这些真正决定体验的地方没有实现。所以本包的每个关键行为
// 都有对应的单元测试, 尤其是打断路径。
package media

import (
	"encoding/binary"
	"math"
)

const (
	// SampleRate 是面试链路的采样率。16kHz 单声道是语音识别的事实标准:
	// 再往上采样对识别准确率几乎没有提升, 只是徒增带宽。
	SampleRate = 16000
	// FrameMS 是音频帧时长。20ms 是 WebRTC 的默认值 —— 足够短以保证打断及时,
	// 足够长以摊薄每个包的协议开销。
	FrameMS = 20
	// FrameSamples 是每帧采样点数。
	FrameSamples = SampleRate * FrameMS / 1000
	// FrameBytes 是每帧字节数(PCM16LE 单声道)。
	FrameBytes = FrameSamples * 2
)

// PCMDurationMS 返回一段 PCM16LE 音频的时长。
func PCMDurationMS(pcm []byte) int64 {
	return int64(len(pcm)/2) * 1000 / SampleRate
}

// RMS 计算 PCM16LE 的均方根能量, 归一化到 [0,1]。
func RMS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		f := float64(s) / 32768.0
		sum += f * f
	}
	return math.Sqrt(sum / float64(n))
}

// ZeroCrossingRate 计算过零率。
//
// 单看能量无法区分"人声"和"持续的背景噪声" —— 空调、风扇、键盘连击
// 的能量都可能高于安静的说话声。过零率提供了第二个维度:
// 语音的浊音段过零率低、清音段高, 而稳定的机械噪声过零率分布很不一样。
func ZeroCrossingRate(pcm []byte) float64 {
	n := len(pcm) / 2
	if n < 2 {
		return 0
	}
	crossings := 0
	prev := int16(binary.LittleEndian.Uint16(pcm))
	for i := 1; i < n; i++ {
		cur := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		if (prev < 0) != (cur < 0) {
			crossings++
		}
		prev = cur
	}
	return float64(crossings) / float64(n-1)
}

// SynthSine 生成正弦波 PCM。仅用于测试与离线演示(模拟人声)。
func SynthSine(ms int, freqHz, amp float64) []byte {
	samples := SampleRate * ms / 1000
	out := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		v := amp * math.Sin(2*math.Pi*freqHz*float64(i)/float64(SampleRate))
		switch {
		case v > 1:
			v = 1
		case v < -1:
			v = -1
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v*32767)))
	}
	return out
}

// SynthSilence 生成静音 PCM。
func SynthSilence(ms int) []byte {
	return make([]byte, SampleRate*ms/1000*2)
}

// SplitFrames 把任意长度的 PCM 切成整帧, 返回完整帧与不足一帧的尾部。
// 网络来的音频不保证按帧对齐, 不做切分会让 VAD 的能量计算抖动。
func SplitFrames(pcm []byte) (frames [][]byte, rest []byte) {
	for len(pcm) >= FrameBytes {
		frames = append(frames, pcm[:FrameBytes])
		pcm = pcm[FrameBytes:]
	}
	return frames, pcm
}
