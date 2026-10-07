// Package response 实现平台统一 HTTP 响应信封 {code, msg, data} 与全局错误出口。
//
// 语义(02 §3.3):HTTP 状态码表达系统健康(200/401/403/429/500),code 表达业务结果;
// 业务错误返回 HTTP 200;认证码 10401~10406 映射 401(触发前端单飞刷新/互斥弹窗/提级弹窗);
// 权限不足 10408 映射 403;限流 110429 映射 429;兜底 500 不泄露内部细节。
//
// 服务 main 启动时调用一次 Setup(),此后 handler 照用 go-zero 原生
// httpx.OkJsonCtx / httpx.ErrorCtx,不再手工包装 w.Write。
package response

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest/httpx"

	"github.com/zxiaosi-micro/micro-common/errcode"
)

// CodeOK 业务成功码。
const CodeOK int64 = 0

// Body 统一响应信封。
type Body struct {
	Code int64  `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

// Success 构造成功信封(0/ok/data)。
func Success(data any) Body { return Body{Code: CodeOK, Msg: "ok", Data: data} }

// Setup 注册全局错误出口与成功信封。服务启动期调用一次;重复调用覆盖先前注册。
func Setup() {
	httpx.SetOkHandler(func(_ context.Context, v any) any {
		return Success(v)
	})
	httpx.SetErrorHandler(ErrorHandler)
}

// ErrorHandler 全局错误出口(02 §3.3 样例实现):
//   - CodeError:业务错误 HTTP 200;认证码 10401~10406 → 401;
//     权限不足 10408 → 403;限流 110429 → 429;
//   - 客户端请求错误(参数/校验):HTTP 200 + 110400(code 表达业务结果);
//   - 其余未知错误:HTTP 500 + 兜底信封,不回传内部细节。
func ErrorHandler(err error) (int, any) {
	var ce *errcode.CodeError
	if errors.As(err, &ce) {
		switch {
		case errcode.IsAuthCode(ce.Code()):
			return http.StatusUnauthorized, ce.Body()
		case ce.Code() == errcode.CodePermissionDenied:
			return http.StatusForbidden, ce.Body()
		case ce.Code() == errcode.CodeRateLimited:
			return http.StatusTooManyRequests, ce.Body()
		default:
			return http.StatusOK, ce.Body()
		}
	}

	if isBadRequest(err) {
		return http.StatusOK, errcode.ErrBadRequest.Body()
	}

	// 未知错误兜底:服务端留痕,对外只出固定信封,不泄露 SQL/拓扑/凭证
	logx.Errorf("http 500 fallback: %v", err)
	return http.StatusInternalServerError, errcode.InternalBody()
}

var badRequestClassifiers []func(error) bool

// SetBadRequestClassifier 追加"客户端请求错误"判定器。
// go-zero httpx.Parse 的 options/range 校验失败无公开哨兵,服务可在此登记
// 自己的判定(如 validator 错误),使其归一为 HTTP 200 + 110400 而非 500。
func SetBadRequestClassifier(classify func(error) bool) {
	badRequestClassifiers = append(badRequestClassifiers, classify)
}

func isBadRequest(err error) bool {
	// 请求体 JSON 语法/类型错误(stdlib 哨兵,httpx.Parse 透传)
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return true
	}
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return true
	}
	for _, classify := range badRequestClassifiers {
		if classify(err) {
			return true
		}
	}
	return false
}
