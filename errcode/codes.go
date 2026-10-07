package errcode

// 认证错误码族(identity 段):BFF/authz/前端请求层共用的契约。
// 10401~10406 在 HTTP 出口映射 401,触发前端对应认证流程
// (10401 静默刷新 / 10402 互斥弹窗 / 10406 提级弹窗,02 §3.3、§9.5)。
const (
	// CodeInvalidCredential 凭证错误(登录失败通用,不区分账号/密码)。
	CodeInvalidCredential int64 = 10400
	// CodeTokenInvalid token 无效或过期 → 前端单飞刷新。
	CodeTokenInvalid int64 = 10401
	// CodeSessionKicked 会话被顶下线(同端互斥)→ 前端互斥弹窗。
	CodeSessionKicked int64 = 10402
	// CodeSessionRevoked 会话已注销/失效(登出、踢出、重用全端注销)。
	CodeSessionRevoked int64 = 10403
	// CodeAccountDisabled 账号禁用/封禁。
	CodeAccountDisabled int64 = 10404
	// CodeSessionExpired 会话已过期(服务端会话寿命终结)。
	CodeSessionExpired int64 = 10405
	// CodeStepUpRequired 需要二级认证 → 前端提级弹窗。
	CodeStepUpRequired int64 = 10406
	// CodeLoginLocked 登录失败锁定(前端倒计时)。
	CodeLoginLocked int64 = 10407
	// CodePermissionDenied 权限不足(RBAC),HTTP 出口映射 403。
	CodePermissionDenied int64 = 10408
)

// AuthCodeMin/AuthCodeMax 认证流程错误码区间(闭区间),HTTP 出口映射 401。
const (
	AuthCodeMin int64 = 10401
	AuthCodeMax int64 = 10406
)

// 平台通用码(platform 段)。
const (
	// CodeInternal 兜底内部错误:对外不泄露内部细节(v1 起沿用 110500)。
	CodeInternal int64 = 110500
	// CodeServiceUnavailable 下游依赖不可用(如 authz fail-close、配置缺失)。
	CodeServiceUnavailable int64 = 110503
	// CodeRateLimited 触发限流(租户 RPM 配额),HTTP 出口映射 429。
	CodeRateLimited int64 = 110429
	// CodeBadRequest 请求参数/校验错误。
	CodeBadRequest int64 = 110400
)

// IsAuthCode 是否属于认证流程错误码区间(10401~10406)。
func IsAuthCode(code int64) bool {
	return code >= AuthCodeMin && code <= AuthCodeMax
}

// AsAuthError 判断 err 是否为认证流程错误(02 §3.3 全局错误出口用)。
func AsAuthError(err error) (*CodeError, bool) {
	ce, ok := FromError(err)
	if !ok || !IsAuthCode(ce.Code()) {
		return nil, false
	}
	return ce, true
}

// —— 平台共用错误码登记(各服务自己的业务码在服务仓 *_err.go 登记) ——

var (
	// ErrTokenInvalid token 无效或过期(触发前端单飞刷新)。
	ErrTokenInvalid = New(SegIdentity, 401, "登录状态已失效")
	// ErrSessionKicked 会话被顶下线(同端互斥踢旧,触发前端互斥弹窗)。
	ErrSessionKicked = New(SegIdentity, 402, "当前账号已在其他设备登录")
	// ErrSessionRevoked 会话已注销或失效。
	ErrSessionRevoked = New(SegIdentity, 403, "会话已失效,请重新登录")
	// ErrAccountDisabled 账号禁用/封禁。
	ErrAccountDisabled = New(SegIdentity, 404, "账号已被禁用")
	// ErrSessionExpired 会话已过期。
	ErrSessionExpired = New(SegIdentity, 405, "会话已过期,请重新登录")
	// ErrStepUpRequired 需要二级认证(触发前端提级弹窗)。
	ErrStepUpRequired = New(SegIdentity, 406, "该操作需要二次验证")
	// ErrLoginLocked 登录失败锁定。
	ErrLoginLocked = New(SegIdentity, 407, "失败次数过多,账号已锁定")
	// ErrPermissionDenied 权限不足(RBAC)。
	ErrPermissionDenied = New(SegIdentity, 408, "没有操作权限")
	// ErrInvalidCredential 凭证错误。
	ErrInvalidCredential = New(SegIdentity, 400, "账号或密码错误")

	// Internal 平台兜底内部错误(对外不泄露内部细节)。
	Internal = New(SegPlatform, 500, "系统繁忙,请稍后重试")
	// ErrServiceUnavailable 下游依赖不可用。
	ErrServiceUnavailable = New(SegPlatform, 503, "服务暂不可用,请稍后重试")
	// ErrRateLimited 触发限流。
	ErrRateLimited = New(SegPlatform, 429, "请求过于频繁,请稍后重试")
	// ErrBadRequest 请求参数/校验错误。
	ErrBadRequest = New(SegPlatform, 400, "请求参数错误")
)

// InternalBody 兜底信封快捷方式(全局错误出口使用)。
func InternalBody() Body { return Internal.Body() }
