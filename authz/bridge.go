package authz

import (
	"context"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/zxiaosi-micro/micro-common/ctxkit"
	"github.com/zxiaosi-micro/micro-common/errcode"
)

// RPC metadata 桥(02 §9.5):
//
//	BFF 侧 ctx 注入(uid/sid/client/roles/tenant/data_scope)后,经
//	OutgoingInterceptor 写入出站 metadata;RPC 服务端经 ServerInterceptor
//	还原回 ctxkit 值——业务 logic 全链路同一套取值,无需参数膨胀。
//
// 同时承担业务码无损往返:服务端把 CodeError 转为 gRPC status(errcode.ToGRPC),
// 客户端把 status 还原为 CodeError(errcode.FromGRPC),logic 层直接 errors.As 消费。

// Metadata 键(与 ctxkit 字段一一对应)。
const (
	MetaUID       = "x-micro-uid"
	MetaSID       = "x-micro-sid"
	MetaClient    = "x-micro-client"
	MetaRoles     = "x-micro-roles" // 逗号连接
	MetaTenant    = "x-micro-tenant"
	MetaDataScope = "x-micro-data-scope" // JSON 原文
)

// OutgoingInterceptor zRPC 客户端出站拦截器:ctxkit 值 → metadata。
// 集成方式:zrpc client Middlewares 挂载本拦截器。
func OutgoingInterceptor(ctx context.Context, method string, req, reply any,
	cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	pairs := make([]string, 0, 12)
	if uid := ctxkit.UID(ctx); uid > 0 {
		pairs = append(pairs, MetaUID, strconv.FormatInt(uid, 10))
	}
	if sid := ctxkit.SID(ctx); sid != "" {
		pairs = append(pairs, MetaSID, sid)
	}
	if client := ctxkit.Client(ctx); client != "" {
		pairs = append(pairs, MetaClient, client)
	}
	if roles := ctxkit.Roles(ctx); len(roles) > 0 {
		pairs = append(pairs, MetaRoles, strings.Join(roles, ","))
	}
	if tid, ok := ctxkit.Tenant(ctx); ok {
		pairs = append(pairs, MetaTenant, strconv.FormatInt(tid, 10))
	}
	if ds := ctxkit.DataScope(ctx); ds != "" {
		pairs = append(pairs, MetaDataScope, ds)
	}
	if len(pairs) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, pairs...)
	}
	err := invoker(ctx, method, req, reply, cc, opts...)
	// 业务码还原:status(code=业务码) → CodeError,logic 层 errors.As 直接消费
	if ce, ok := errcode.FromGRPC(err); ok {
		return ce
	}
	return err
}

// ServerInterceptor RPC 服务端入站拦截器:metadata → ctxkit 值,
// 并在出口把 CodeError 转为 gRPC status(业务码无损上行)。
func ServerInterceptor(ctx context.Context, req any,
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = restoreContext(ctx, md)
	}
	resp, err := handler(ctx, req)
	if err != nil {
		if ce, ok := errcode.FromError(err); ok {
			return resp, ce.ToGRPC()
		}
	}
	return resp, err
}

func restoreContext(ctx context.Context, md metadata.MD) context.Context {
	get := func(key string) string {
		if vs := md.Get(key); len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	if v := get(MetaUID); v != "" {
		if uid, err := strconv.ParseInt(v, 10, 64); err == nil && uid > 0 {
			ctx = ctxkit.WithUID(ctx, uid)
		}
	}
	if v := get(MetaSID); v != "" {
		ctx = ctxkit.WithSID(ctx, v)
	}
	if v := get(MetaClient); v != "" {
		ctx = ctxkit.WithClient(ctx, v)
	}
	if v := get(MetaRoles); v != "" {
		ctx = ctxkit.WithRoles(ctx, strings.Split(v, ","))
	}
	if v := get(MetaTenant); v != "" {
		if tid, err := strconv.ParseInt(v, 10, 64); err == nil && tid > 0 {
			ctx = ctxkit.WithTenant(ctx, tid)
		}
	}
	if v := get(MetaDataScope); v != "" {
		ctx = ctxkit.WithDataScope(ctx, v)
	}
	return ctx
}
