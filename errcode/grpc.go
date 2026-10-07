package errcode

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToGRPC 把业务错误编码为 gRPC status error:
// 利用 status code 允许任意 uint32 的特性,完整业务码(≥10000)直接作 status code,
// msg 作 status message——跨服务 RPC 传输不丢码、不丢消息。
// 非 CodeError 返回 nil(调用方自行决定底层错误如何传输)。
func (e *CodeError) ToGRPC() error {
	if e == nil {
		return nil
	}
	return status.New(codes.Code(uint32(e.code)), e.msg).Err()
}

// FromGRPC 从 gRPC error 还原业务错误(status code 落在已注册段才算业务码,
// 其余——如 codes.Unavailable/Canceled 等传输态错误——返回 false,由调用方兜底)。
func FromGRPC(err error) (*CodeError, bool) {
	if err == nil {
		return nil, false
	}
	st, ok := status.FromError(err)
	if !ok {
		// 非 status 错误:也可能是被包装的 status.FromContextError 等,统一按非业务码处理
		return nil, false
	}
	code := int64(st.Code())
	if code < int64(SegIdentity) {
		return nil, false
	}
	ce, err2 := NewFull(code, st.Message())
	if err2 != nil {
		return nil, false
	}
	return ce, true
}

// ToGRPCStatus 返回底层 *status.Status(需要 details/元数据的场景)。
func (e *CodeError) ToGRPCStatus() *status.Status {
	return status.New(codes.Code(uint32(e.code)), e.msg)
}

// WrapGRPC 为底层错误附加业务码并转 gRPC error(服务端 logic 出口用)。
func WrapGRPC(cause error, seg Segment, code int, msg string) error {
	return Wrap(cause, seg, code, msg).ToGRPC()
}

var _ error = (*CodeError)(nil)

// 约束:业务码必须能无损放入 gRPC status code(uint32,且避开 0~15 标准码区)。
func init() {
	maxCode := int64(SegPlatform) + segmentSpan - 1
	if maxCode > int64(^uint32(0)>>1) {
		panic(fmt.Sprintf("errcode: 最大业务码 %d 超出 gRPC status code 安全范围", maxCode))
	}
}
