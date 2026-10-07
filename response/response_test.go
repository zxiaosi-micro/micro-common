package response

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/rest/httpx"

	"github.com/zxiaosi-micro/micro-common/errcode"
)

func TestErrorHandlerBusinessErrorHTTP200(t *testing.T) {
	status_, body := ErrorHandler(errcode.New(errcode.SegInventory, 23, "库存不足"))
	if status_ != http.StatusOK {
		t.Fatalf("业务错误应 HTTP 200, got %d", status_)
	}
	b, ok := body.(errcode.Body)
	if !ok || b.Code != 40023 || b.Msg != "库存不足" {
		t.Fatalf("body = %#v", body)
	}
}

func TestErrorHandlerAuthCodesTo401(t *testing.T) {
	// 10401~10406 → 401,触发前端单飞刷新/互斥弹窗/提级弹窗
	for _, ce := range []*errcode.CodeError{
		errcode.ErrTokenInvalid, errcode.ErrSessionKicked, errcode.ErrSessionRevoked,
		errcode.ErrAccountDisabled, errcode.ErrSessionExpired, errcode.ErrStepUpRequired,
	} {
		status_, _ := ErrorHandler(fmt.Errorf("wrap: %w", ce))
		if status_ != http.StatusUnauthorized {
			t.Fatalf("认证码 %d 应映射 401, got %d", ce.Code(), status_)
		}
	}
	// 10400 凭证错误与 10407 锁定是登录页分流,不在 401 区间
	for _, ce := range []*errcode.CodeError{errcode.ErrInvalidCredential, errcode.ErrLoginLocked} {
		status_, _ := ErrorHandler(ce)
		if status_ != http.StatusOK {
			t.Fatalf("码 %d 应为 HTTP 200, got %d", ce.Code(), status_)
		}
	}
}

func TestErrorHandlerPermissionDeniedTo403(t *testing.T) {
	status_, _ := ErrorHandler(errcode.ErrPermissionDenied)
	if status_ != http.StatusForbidden {
		t.Fatalf("权限不足应映射 403, got %d", status_)
	}
}

func TestErrorHandlerRateLimitedTo429(t *testing.T) {
	status_, _ := ErrorHandler(errcode.ErrRateLimited)
	if status_ != http.StatusTooManyRequests {
		t.Fatalf("限流应映射 429, got %d", status_)
	}
}

func TestErrorHandlerFallbackNoLeak(t *testing.T) {
	secret := "mysql://root:p@ssw0rd@10.0.0.8:3306/identity"
	status_, body := ErrorHandler(fmt.Errorf("dial tcp: %s", secret))
	if status_ != http.StatusInternalServerError {
		t.Fatalf("未知错误应映射 500, got %d", status_)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("兜底信封序列化失败: %v", err)
	}
	// 关键断言:内部细节不进响应体
	if strings.Contains(string(b), secret) {
		t.Fatalf("兜底信封泄露内部细节: %s", b)
	}
	var bb errcode.Body
	if err := json.Unmarshal(b, &bb); err != nil || bb.Code != errcode.CodeInternal {
		t.Fatalf("兜底信封应为 110500: %s %v", b, err)
	}
}

func TestErrorHandlerBadRequest(t *testing.T) {
	// stdlib json 哨兵(httpx.Parse 透传)→ HTTP 200 + 110400
	syntaxErr := &json.SyntaxError{Offset: 3}
	status_, body := ErrorHandler(fmt.Errorf("parse: %w", syntaxErr))
	if status_ != http.StatusOK {
		t.Fatalf("请求体错误应 HTTP 200, got %d", status_)
	}
	if b := body.(errcode.Body); b.Code != errcode.CodeBadRequest {
		t.Fatalf("应返回 110400, got %d", b.Code)
	}
	// 自定义判定器
	errValidationDummy := errors.New("dummy validation failure")
	SetBadRequestClassifier(func(err error) bool { return errors.Is(err, errValidationDummy) })
	status_, _ = ErrorHandler(errValidationDummy)
	if status_ != http.StatusOK {
		t.Fatalf("自定义判定应 HTTP 200, got %d", status_)
	}
}

// TestEndToEndViaHttpx 走真实 httpx 全局出口验证(Setup 后由框架调用 ErrorHandler)。
func TestEndToEndViaHttpx(t *testing.T) {
	Setup()

	// 成功信封:{code:0,msg:"ok",data}
	rec := httptest.NewRecorder()
	httpx.OkJsonCtx(t.Context(), rec, map[string]string{"hello": "world"})
	if rec.Code != http.StatusOK {
		t.Fatalf("成功响应应 200, got %d", rec.Code)
	}
	var okBody Body
	if err := json.Unmarshal(rec.Body.Bytes(), &okBody); err != nil {
		t.Fatalf("成功信封解析失败: %v", err)
	}
	if okBody.Code != CodeOK || okBody.Msg != "ok" || okBody.Data == nil {
		t.Fatalf("成功信封 = %s", rec.Body.String())
	}

	// 错误出口:认证错误 HTTP 401 + {code,msg}
	rec2 := httptest.NewRecorder()
	httpx.ErrorCtx(t.Context(), rec2, errcode.ErrSessionKicked)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("10402 应映射 401, got %d", rec2.Code)
	}
	var errBody errcode.Body
	if err := json.Unmarshal(rec2.Body.Bytes(), &errBody); err != nil || errBody.Code != errcode.CodeSessionKicked {
		t.Fatalf("错误信封 = %s", rec2.Body.String())
	}
}
