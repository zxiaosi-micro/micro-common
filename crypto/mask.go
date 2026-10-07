package crypto

import "strings"

// 统一掩码函数(02 §13):展示与日志共用同一套规则,
// 各服务不得自造掩码格式,避免同字段在页面上出现两种脱敏样式。

// MaskPhone 手机号:保留前 3 后 4(138****1234);长度不足时整体掩码。
func MaskPhone(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 7 {
		return maskAll(s)
	}
	return s[:3] + "****" + s[len(s)-4:]
}

// MaskEmail 邮箱:保留 @ 前首字符与完整域名(a***@example.com)。
func MaskEmail(s string) string {
	s = strings.TrimSpace(s)
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || domain == "" {
		return maskAll(s)
	}
	return local[:1] + "***@" + domain
}

// MaskBankCard 银行卡:保留前 4 后 4;过短整体掩码。
func MaskBankCard(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return maskAll(s)
	}
	return s[:4] + " **** " + s[len(s)-4:]
}

// MaskIDCard 身份证:保留前 3 后 4,中间固定 11 星;过短整体掩码。
func MaskIDCard(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return maskAll(s)
	}
	return s[:3] + "***********" + s[len(s)-4:]
}

// MaskName 姓名:保留姓氏(首个字符),其余按字符数掩码;单字名整体掩码。
func MaskName(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= 1 {
		return maskAll(s)
	}
	return string(runes[0]) + strings.Repeat("*", len(runes)-1)
}

// MaskDefault 通用掩码:保留首尾各 1 字符(rune 级,兼容多字节),中间 ***;
// 过短整体掩码。
func MaskDefault(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) < 2 {
		return maskAll(s)
	}
	return string(runes[0]) + "***" + string(runes[len(runes)-1])
}

func maskAll(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}
