// Package privacy 提供个人信息的脱敏与假名化。
//
// 招聘系统存的是最敏感的一类个人信息: 姓名、简历、录音、评分、来源 IP。
// 这里实现两条底线能力:
//
//   - 假名化(pseudonymisation): 客户的候选人 ID 不落明文, 只落不可逆的引用值,
//     既能做跨表关联与聚合统计, 又不构成"可直接识别个人"的数据;
//   - 脱敏(masking): 来源 IP、UA 这类取证信息保留到"足够证明发生了什么"的程度,
//     不保留完整精度。
//
// 这不是可选项。设计文档里承诺了"手机号/邮箱脱敏存储 + 唯一哈希",
// 代码就必须真的做到 —— 对客户做出的合规承诺无法兑现, 比不做承诺更危险。
package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
)

// Ref 计算候选人引用值。
//
// 用 HMAC 而不是裸 SHA256: 候选人 ID 往往是"手机号后四位+日期"这类低熵值,
// 裸哈希可以被彩虹表直接反查, 加上租户维度的密钥才是真正的假名化。
func Ref(secret []byte, tenantID, rawID string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(tenantID))
	m.Write([]byte{0})
	m.Write([]byte(strings.TrimSpace(rawID)))
	return "cand_" + hex.EncodeToString(m.Sum(nil))[:32]
}

// MaskIP 把 IP 脱敏到网段级别。
//
// IPv4 保留前三段(203.0.113.x), IPv6 保留前 48 位。这个精度足够证明
// "某次授权来自哪个网络", 又不足以定位到具体个人 —— 合规取证要的是
// 前者, 不是后者。
func MaskIP(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "invalid"
	}
	if v4 := ip.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], 0).String()[:len(net.IPv4(v4[0], v4[1], v4[2], 0).String())-1] + "x"
	}
	// IPv6: 只保留前三个 16 位分组
	full := ip.String()
	parts := strings.Split(full, ":")
	if len(parts) >= 3 {
		return strings.Join(parts[:3], ":") + "::/48"
	}
	return "::/48"
}

// MaskUserAgent 截断 UA, 只保留客户端与主版本这类排查所需的信息。
func MaskUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) <= 80 {
		return ua
	}
	return ua[:80] + "…"
}

// RedactName 返回姓名用于列表展示的脱敏形式(保留姓氏)。
func RedactName(name string) string {
	rs := []rune(strings.TrimSpace(name))
	switch {
	case len(rs) == 0:
		return "候选人"
	case len(rs) == 1:
		return string(rs)
	default:
		return string(rs[0]) + strings.Repeat("*", len(rs)-1)
	}
}

// MaskEmail 保留邮箱首字符与域名, 中间打码。
//
// 招聘系统需要"能联系到人", 但不需要把完整邮箱写给每一个能看列表的人。
// "c***@example.com" 足够确认身份与域名, 又不足以直接拿去群发或撞库。
func MaskEmail(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	at := strings.LastIndex(raw, "@")
	if at <= 0 || at == len(raw)-1 {
		return maskMiddle(raw, 1, 2)
	}
	local := []rune(raw[:at])
	domain := raw[at+1:]
	if len(local) == 1 {
		return string(local) + "***@" + domain
	}
	return string(local[0]) + strings.Repeat("*", len(local)-1) + "@" + domain
}

// MaskPhone 保留前 3 位与后 4 位, 中间打码。
//
// 保留后四位是有意的: 客服与候选人核对身份时通常就是核这四位,
// 而中间四位才是能唯一锁定个人的部分。
func MaskPhone(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	digits := make([]rune, 0, len(raw))
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 7 {
		return maskMiddle(raw, 1, 1)
	}
	return string(digits[:3]) + "****" + string(digits[len(digits)-4:])
}

// maskMiddle 是通用的中间打码: 保留头部 head 个字符与尾部 tail 个字符。
func maskMiddle(s string, head, tail int) string {
	rs := []rune(s)
	if len(rs) <= head+tail {
		return string(rs[:1]) + strings.Repeat("*", max(1, len(rs)-1))
	}
	return string(rs[:head]) + strings.Repeat("*", len(rs)-head-tail) + string(rs[len(rs)-tail:])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
