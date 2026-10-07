package errcode

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSegmentRegistry(t *testing.T) {
	cases := []struct {
		code int64
		dom  string
	}{
		{10400, "identity"},
		{10401, "identity"},
		{20001, "party"},
		{110500, "platform"},
	}
	for _, c := range cases {
		if got := DomainOf(c.code); got != c.dom {
			t.Errorf("DomainOf(%d)=%q, want %q", c.code, got, c.dom)
		}
	}
	// gRPC 标准码区与未注册段不落域
	if DomainOf(3) != "" || DomainOf(999) != "" || DomainOf(500000) != "" {
		t.Error("标准码区/未注册段不应落域")
	}
	if _, ok := SegmentOf(110499); !ok {
		t.Error("platform 段末位应可解析")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("重复注册段应 panic")
		}
	}()
	Register(SegIdentity, "another")
}

func TestRegisterInvalidPanics(t *testing.T) {
	for _, seg := range []Segment{0, 15000, -10000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("段 %d 非法应 panic", seg)
				}
			}()
			Register(seg, "x")
		}()
	}
}

func TestNewAndBody(t *testing.T) {
	ce := New(SegInventory, 23, "库存不足")
	if ce.Code() != 40023 {
		t.Fatalf("code = %d, want 40023", ce.Code())
	}
	b := ce.Body()
	if b.Code != 40023 || b.Msg != "库存不足" {
		t.Fatalf("body = %+v", b)
	}
	if ce.Error() != "[40023] 库存不足" {
		t.Fatalf("Error() = %q", ce.Error())
	}
}

func TestNewInvalidPanics(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("未注册段应 panic")
			}
		}()
		_ = New(Segment(999000), 1, "x")
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("偏移越界应 panic")
			}
		}()
		_ = New(SegOrder, 10000, "x")
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("空消息应 panic")
			}
		}()
		_ = New(SegOrder, 1, "")
	}()
}

func TestWrapAndUnwrap(t *testing.T) {
	cause := errors.New("redis timeout")
	ce := Wrap(cause, SegInventory, 23, "库存不足")
	if !errors.Is(ce, cause) {
		t.Fatal("Wrap 后应能 errors.Is 穿透到底层错误")
	}
	got, ok := FromError(fmt.Errorf("reserve: %w", ce))
	if !ok || got.Code() != 40023 {
		t.Fatalf("FromError 还原失败: %+v ok=%v", got, ok)
	}
	if _, ok := FromError(errors.New("plain")); ok {
		t.Fatal("普通错误不应还原出 CodeError")
	}
}

func TestGRPCRoundTripLossless(t *testing.T) {
	// 验收口径:业务码跨 gRPC status 无损往返(码与消息均不丢失)
	for _, ce := range []*CodeError{
		New(SegIdentity, 401, "登录状态已失效"),
		New(SegInventory, 23, "库存不足"),
		New(SegFinance, 9999, "渠道异常"),
		New(SegPlatform, 500, "系统繁忙,请稍后重试"),
	} {
		wire := ce.ToGRPC()
		back, ok := FromGRPC(wire)
		if !ok {
			t.Fatalf("业务码 %d 未能从 gRPC status 还原", ce.Code())
		}
		if back.Code() != ce.Code() || back.Msg() != ce.Msg() {
			t.Fatalf("往返有损: want(%d,%s) got(%d,%s)",
				ce.Code(), ce.Msg(), back.Code(), back.Msg())
		}
	}
}

func TestGRPCRoundTripThroughSimulatedTransport(t *testing.T) {
	// 模拟真实跨服务链路:服务端 logic 返回 CodeError → 服务端出口 ToGRPC →
	// 网络传输(status error)→ 客户端 FromGRPC 还原
	orig := Wrap(errors.New("conn refused"), SegOrder, 507, "下单失败")
	wire := orig.ToGRPC()
	back, ok := FromGRPC(wire)
	if !ok || back.Code() != orig.Code() || back.Msg() != orig.Msg() {
		t.Fatalf("传输后还原有损: %+v ok=%v", back, ok)
	}
	// 客户端 SDK 对 status error 再包装的场景:业务码仍无损
	// (grpc-go 会把包装层上下文并入 status message,码不丢)
	transported := fmt.Errorf("call order.Create: %w", wire)
	back2, ok := FromGRPC(transported)
	if !ok || back2.Code() != orig.Code() {
		t.Fatalf("包装传输后业务码丢失: %+v ok=%v", back2, ok)
	}
}

func TestFromGRPCRejectsNonBusiness(t *testing.T) {
	for _, err := range []error{
		status.Error(codes.Unavailable, "conn refused"),
		status.Error(codes.InvalidArgument, "bad arg"),
		errors.New("plain"),
		nil,
	} {
		if _, ok := FromGRPC(err); ok {
			t.Fatalf("非业务码错误不应还原: %v", err)
		}
	}
}

func TestNewFull(t *testing.T) {
	ce, err := NewFull(10402, "被顶下线")
	if err != nil || ce.Code() != 10402 {
		t.Fatalf("NewFull 失败: %v %v", ce, err)
	}
	if _, err := NewFull(3, "x"); err == nil {
		t.Fatal("标准 gRPC 码不应构造成业务错误")
	}
	if _, err := NewFull(500000, "x"); err == nil {
		t.Fatal("未注册段不应构造成业务错误")
	}
}

func TestAuthCodes(t *testing.T) {
	for code := int64(10401); code <= 10406; code++ {
		if !IsAuthCode(code) {
			t.Fatalf("%d 应属于认证码区间", code)
		}
	}
	for _, code := range []int64{10400, 10407, 10408, 0, 110500} {
		if IsAuthCode(code) {
			t.Fatalf("%d 不应属于认证码区间", code)
		}
	}
	if ce, ok := AsAuthError(ErrSessionKicked); !ok || ce.Code() != CodeSessionKicked {
		t.Fatal("AsAuthError 应识别 10402")
	}
	if _, ok := AsAuthError(ErrPermissionDenied); ok {
		t.Fatal("10408 是权限码,不属于 401 认证码区间")
	}
}

func TestWithMsgAndWithCause(t *testing.T) {
	base := ErrLoginLocked
	dyn := base.WithMsg("失败次数过多,账号已锁定 1500 秒")
	if dyn.Msg() == base.Msg() || dyn.Code() != base.Code() {
		t.Fatal("WithMsg 应只改消息不改码")
	}
	if base.Msg() == "失败次数过多,账号已锁定 1500 秒" {
		t.Fatal("原错误消息不应被修改")
	}
	w := newStockErrWithCause()
	if w.cause == nil {
		t.Fatal("WithCause 应保留 cause")
	}
}

// 辅助:模拟服务侧的链式用法
func newStockErrWithCause() *CodeError {
	return New(SegInventory, 23, "库存不足").WithCause(errors.New("available=0"))
}
