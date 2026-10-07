package crypto

import (
	"reflect"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
)

// 日志脱敏框架机制(02 §11/§13):
//
//	go-zero v1.9.0+ 的 logx.Sensitive 接口——实现 MaskSensitive() any 的结构体,
//	在 logx Infov/Errorv 及 LogField 输出时自动脱敏。
//	本包提供声明式适配器:服务结构体不必手写 MaskSensitive,
//	用 SensitiveOf(v) 包装即可按默认/自定义字段规则掩码。
//
//	type User struct {
//	    Mobile string `json:"mobile"`
//	    Email  string `json:"email"`
//	}
//	logx.Infow("user login", logx.Field("user", crypto.SensitiveOf(user)))

// Sensitive 对齐 go-zero logx 的脱敏接口。
type Sensitive = logx.Sensitive

// 编译期约束:适配器必须满足 logx.Sensitive。
var _ Sensitive = (*MaskedView)(nil)

// FieldRule 字段掩码规则。
type FieldRule func(s string) string

// DefaultFieldRules 默认字段规则(按字段名小写匹配)。
var DefaultFieldRules = map[string]FieldRule{
	"mobile":       MaskPhone,
	"phone":        MaskPhone,
	"email":        MaskEmail,
	"bankcard":     MaskBankCard,
	"cardno":       MaskBankCard,
	"idcard":       MaskIDCard,
	"realname":     MaskName,
	"name":         MaskName,
	"username":     MaskName,
	"password":     func(string) string { return "***" },
	"secret":       func(string) string { return "***" },
	"accesstoken":  func(string) string { return "***" },
	"refreshtoken": func(string) string { return "***" },
}

// MaskedView 结构体掩码视图,实现 logx.Sensitive。
type MaskedView struct {
	inner any
	rules map[string]FieldRule
}

// SensitiveOf 以默认字段规则包装结构体,返回 logx 可自动脱敏的视图。
// 非结构体输入原样返回包装(输出时保持原值)。
func SensitiveOf(v any) *MaskedView {
	return &MaskedView{inner: v, rules: DefaultFieldRules}
}

// SensitiveOfWithRules 以自定义字段规则包装(叠加在默认规则之上,可覆盖)。
func SensitiveOfWithRules(v any, rules map[string]FieldRule) *MaskedView {
	merged := make(map[string]FieldRule, len(DefaultFieldRules)+len(rules))
	for k, f := range DefaultFieldRules {
		merged[k] = f
	}
	for k, f := range rules {
		merged[strings.ToLower(k)] = f
	}
	return &MaskedView{inner: v, rules: merged}
}

// MaskSensitive 实现 logx.Sensitive:返回按规则掩码后的结构体浅拷贝。
func (m *MaskedView) MaskSensitive() any {
	if m == nil || m.inner == nil {
		return m.inner
	}
	rv := reflect.ValueOf(m.inner)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return m.inner
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return m.inner
	}
	// 浅拷贝结构体,仅替换命中规则的 string 字段
	masked := reflect.New(rv.Type()).Elem()
	masked.Set(rv)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if field.PkgPath != "" { // 非导出字段跳过
			continue
		}
		rule, ok := m.rules[strings.ToLower(field.Name)]
		if !ok {
			continue
		}
		fv := masked.Field(i)
		if fv.Kind() != reflect.String || !fv.CanSet() {
			continue
		}
		fv.SetString(rule(fv.String()))
	}
	if reflect.ValueOf(m.inner).Kind() == reflect.Pointer {
		return masked.Addr().Interface()
	}
	return masked.Interface()
}
