package account

// minLivenessMotion 是相邻帧之间要求的最小变化量。
//
// 只拒绝"完全相同"的帧: 程序化地把同一帧重复提交时余弦相似度恒为 1,
// 变化量为 0; 真实摄像头即使对着静止的人也会因传感器噪声产生微小差异。
// 这个值刻意取得极小, 宁可漏掉重放攻击, 也不误伤正常用户 —— 真正的
// 活体必须靠厂商模型(动作/红外/3D), 不能靠调这个阈值。
const minLivenessMotion = 1e-9

// livenessScore 返回相邻帧之间的平均变化量(0=完全静止, 1=完全不同)。
//
// 这是开发级活体: 它能挡住"把一张照片当成多帧提交"的最简单攻击,
// 但挡不住"用手机播放一段真人视频"的重放攻击。真正的活体需要厂商的
// 动作/红外/3D 结构光能力, 因此 FaceProfile.Assurance 仍然如实标注
// development-only。
func (s *Service) livenessScore(vecs [][]float32) float64 {
	if len(vecs) < 2 {
		return 0
	}
	var sum float64
	for i := 1; i < len(vecs); i++ {
		sum += 1 - s.matcher.Compare(vecs[i-1], vecs[i])
	}
	return sum / float64(len(vecs)-1)
}
