// Package ctxkit 提供横切上下文(uid/会话/登录端/角色/租户/数据域)的统一存取。
//
// 纪律(02 §3.5):BFF 中间件注入一次,全链路(HTTP→RPC→MQ)透传,
// 杜绝函数参数膨胀;RPC 侧经 authz 的 metadata 桥还原为本包取值。
package ctxkit

import "context"

// 登录端标识(identity 契约,新增端先在此登记,S3/S9)。
const (
	ClientAdminWeb    = "ADMIN_WEB"    // 管理后台 Web
	ClientDealerWeb   = "DEALER_WEB"   // 经销商 Web
	ClientStationWeb  = "STATION_WEB"  // 场站 Web
	ClientClientMini  = "CLIENT_MINI"  // 客户端小程序
	ClientStationMini = "STATION_MINI" // 场站小程序
	ClientOpsApp      = "OPS_APP"      // 运维 App
)

type ctxKey uint8

const (
	keyUID ctxKey = iota + 1
	keySID
	keyClient
	keyRoles
	keyTenant
	keyDataScope
)

// WithUID 注入操作人用户 ID(雪花 int64)。
func WithUID(ctx context.Context, uid int64) context.Context {
	return context.WithValue(ctx, keyUID, uid)
}

// UID 取操作人 ID;未注入返回 0。
func UID(ctx context.Context) int64 {
	v, _ := ctx.Value(keyUID).(int64)
	return v
}

// WithSID 注入会话 ID。
func WithSID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, keySID, sid)
}

// SID 取会话 ID;未注入返回空串。
func SID(ctx context.Context) string {
	v, _ := ctx.Value(keySID).(string)
	return v
}

// WithClient 注入登录端(值域见本包 Client* 常量)。
func WithClient(ctx context.Context, client string) context.Context {
	return context.WithValue(ctx, keyClient, client)
}

// Client 取登录端;未注入返回空串。
func Client(ctx context.Context) string {
	v, _ := ctx.Value(keyClient).(string)
	return v
}

// WithRoles 注入角色码集合(auth_cache 快照)。
func WithRoles(ctx context.Context, roles []string) context.Context {
	return context.WithValue(ctx, keyRoles, roles)
}

// Roles 取角色码集合;未注入返回 nil。
func Roles(ctx context.Context) []string {
	v, _ := ctx.Value(keyRoles).([]string)
	return v
}

// WithTenant 注入租户 ID;业务查询的租户过滤一律显式携带该值(ADR-08)。
func WithTenant(ctx context.Context, tenantID int64) context.Context {
	return context.WithValue(ctx, keyTenant, tenantID)
}

// Tenant 取租户 ID;未注入返回 (0, false)。
// 业务 Logic 层需要强约束时用 tenantx.MustTenantFromCtx(fail-closed)。
func Tenant(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(keyTenant).(int64)
	return v, ok
}

// WithDataScope 注入数据域(JSON 串,与 role.data_scope 同构;空串=不限制)。
func WithDataScope(ctx context.Context, dataScope string) context.Context {
	return context.WithValue(ctx, keyDataScope, dataScope)
}

// DataScope 取数据域;未注入返回空串。
func DataScope(ctx context.Context) string {
	v, _ := ctx.Value(keyDataScope).(string)
	return v
}
