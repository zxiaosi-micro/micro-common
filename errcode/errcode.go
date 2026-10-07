// Package errcode 实现平台统一的业务错误码:万位段分段注册 + gRPC status 无损往返。
//
// 设计(02 技术文档 §8 / ADR):
//   - 每个域占一个万位段(identity=10000 … platform=110000),看码即知域;
//   - 各服务在自己的 *_err.go 中用 New/Wrap 集中登记,禁止裸写数字;
//   - 利用 gRPC status code 允许任意 uint32 的特性,业务码经 ToGRPC/FromGRPC
//     跨服务传输不丢码、不丢消息。
package errcode

import (
	"errors"
	"fmt"
)

// Segment 错误码段:每域一个万位段(步长 10000)。
type Segment int64

// 平台既有域段(02 §8,新增域按 10000 步长续接并在此登记)。
const (
	SegIdentity  Segment = 10000  // 认证与账号(identity)
	SegParty     Segment = 20000  // 参与方(party)
	SegCatalog   Segment = 30000  // 商品(catalog)
	SegInventory Segment = 40000  // 库存(inventory)
	SegOrder     Segment = 50000  // 订单(order)
	SegContract  Segment = 60000  // 合同质保(contract)
	SegStation   Segment = 70000  // 场站(station)
	SegDevice    Segment = 80000  // 设备与 IoT(device/iotingest)
	SegOps       Segment = 90000  // 运维(ops)
	SegFinance   Segment = 100000 // 财务(finance)
	SegPlatform  Segment = 110000 // 平台通用(兜底/限流等)
)

// segmentSpan 每段宽度:段内偏移 0~9999。
const segmentSpan int64 = 10000

// CodeError 携带业务码的错误,可实现 errors.Is/As 链路传递。
type CodeError struct {
	code  int64
	msg   string
	cause error
}

// Body 业务错误对外信封(code+msg,不含内部细节)。
type Body struct {
	Code int64  `json:"code"`
	Msg  string `json:"msg"`
}

func (e *CodeError) Code() int64 { return e.code }

// Msg 返回对外消息;动态上下文(如锁定剩余秒数)用 WithMsg 克隆后覆写。
func (e *CodeError) Msg() string { return e.msg }

// Body 返回对外信封。
func (e *CodeError) Body() Body { return Body{Code: e.code, Msg: e.msg} }

func (e *CodeError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.code, e.msg, e.cause)
	}
	return fmt.Sprintf("[%d] %s", e.code, e.msg)
}

// Unwrap 暴露底层 cause,支持 errors.Is/As 穿透。
func (e *CodeError) Unwrap() error { return e.cause }

// WithCause 附加底层错误(不改变业务码与消息),用于 Wrap 场景的链式书写:
// errcode.ErrStockInsufficient.WithCause(err)。
func (e *CodeError) WithCause(cause error) *CodeError {
	clone := *e
	clone.cause = cause
	return &clone
}

// WithMsg 克隆并覆写对外消息(如带剩余秒数的锁定提示)。
func (e *CodeError) WithMsg(msg string) *CodeError {
	clone := *e
	clone.msg = msg
	return &clone
}

// New 登记一个业务错误:code 为段内偏移(0~9999),完整业务码 = seg+code。
// 段/偏移非法或段未注册时 panic——登记错误应在启动期暴露,不允许静默。
func New(seg Segment, code int, msg string) *CodeError {
	return newCodeError(int64(seg), code, msg)
}

// Wrap 同 New,并附加底层错误 cause(日志与对账用它保留上下文,对外仍只出 code/msg)。
func Wrap(cause error, seg Segment, code int, msg string) *CodeError {
	return New(seg, code, msg).WithCause(cause)
}

// NewFull 用完整业务码构造(FromGRPC/FromError 还原场景)。
// 非法码(未落任何已注册段)返回 error 而不 panic。
func NewFull(fullCode int64, msg string) (*CodeError, error) {
	if _, ok := segmentOf(fullCode); !ok {
		return nil, fmt.Errorf("errcode: 业务码 %d 不在任何已注册段内", fullCode)
	}
	return &CodeError{code: fullCode, msg: msg}, nil
}

// FromError 从错误链中还原 CodeError(errors.As)。
func FromError(err error) (*CodeError, bool) {
	var ce *CodeError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

func newCodeError(full int64, offset int, msg string) *CodeError {
	if _, ok := segmentOf(full); !ok {
		panic(fmt.Sprintf("errcode: 段 %d 未注册或非法", full))
	}
	if offset < 0 || offset >= int(segmentSpan) {
		panic(fmt.Sprintf("errcode: 段内偏移 %d 越界(0~9999)", offset))
	}
	if msg == "" {
		panic("errcode: 业务码消息不允许为空(禁止无语义错误码)")
	}
	return &CodeError{code: full + int64(offset), msg: msg}
}

// —— 段注册表:看码知域的依据 ——
// 直接以字面量初始化(Go 按依赖序保证先于 codes.go 的错误变量构造执行);
// 后续服务可用 Register 续接自己的段。

var registeredSegments = map[Segment]string{
	SegIdentity:  "identity",
	SegParty:     "party",
	SegCatalog:   "catalog",
	SegInventory: "inventory",
	SegOrder:     "order",
	SegContract:  "contract",
	SegStation:   "station",
	SegDevice:    "device",
	SegOps:       "ops",
	SegFinance:   "finance",
	SegPlatform:  "platform",
}

// Register 登记一个段;重复登记或非法段 panic(多服务并发注册撞段时启动即失败)。
func Register(seg Segment, name string) {
	if seg < SegIdentity || seg%Segment(segmentSpan) != 0 {
		panic(fmt.Sprintf("errcode: 段 %d 非法(必须为 ≥10000 的万位段)", seg))
	}
	if name == "" {
		panic("errcode: 段名不允许为空")
	}
	if _, dup := registeredSegments[seg]; dup {
		panic(fmt.Sprintf("errcode: 段 %d 重复注册", seg))
	}
	registeredSegments[seg] = name
}

// SegmentOf 返回业务码所属段;未落段的码返回 false。
func SegmentOf(code int64) (Segment, bool) {
	return segmentOf(code)
}

// DomainOf 返回业务码所属域名(未注册码返回空串)。
func DomainOf(code int64) string {
	if seg, ok := segmentOf(code); ok {
		return registeredSegments[seg]
	}
	return ""
}

func segmentOf(code int64) (Segment, bool) {
	if code < int64(SegIdentity) {
		return 0, false
	}
	seg := Segment(code / segmentSpan * segmentSpan)
	if code >= int64(seg)+segmentSpan {
		return 0, false
	}
	if _, ok := registeredSegments[seg]; !ok {
		return 0, false
	}
	return seg, true
}
