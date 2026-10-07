package tenantx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zeromicro/go-zero/core/limit"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"github.com/zxiaosi-micro/micro-common/errcode"
)

// 租户 RPM 分钟桶配额(02 §9.4):
//   - go-zero limit.PeriodLimit(60s 分钟桶),per-tenant 配额由调用方解析(tenant 表 plan.quota);
//   - default 租户与 quota=0 不限;
//   - 超限返回 429 语义 + RateLimit 头,HTTP 出口由 tenantx.WriteRateLimited 短路写出。

// DefaultTenantCode default 租户(不限流)。
const DefaultTenantCode = "default"

// RateLimitHeader 限流响应头。
const (
	HeaderRateLimitLimit     = "X-RateLimit-Limit"
	HeaderRateLimitRemaining = "X-RateLimit-Remaining"
	HeaderRetryAfter         = "Retry-After"
)

// QuotaResolver 返回租户的 RPM 配额;0 = 不限。
type QuotaResolver func(ctx context.Context, tenantID int64) int

// QuotaLimiter 租户 RPM 配额器。
type QuotaLimiter struct {
	store      *redis.Redis
	keyPrefix  string
	resolve    QuotaResolver
	periodSecs int
}

// NewQuotaLimiter 构造:keyPrefix 建议含服务名(如 "micro:order:quota:"),
// resolve 由服务接租户配额存储(tenant 表/缓存)。
func NewQuotaLimiter(store *redis.Redis, keyPrefix string, resolve QuotaResolver) (*QuotaLimiter, error) {
	if store == nil {
		return nil, fmt.Errorf("tenantx: redis store 不允许为空")
	}
	if keyPrefix == "" {
		keyPrefix = "micro:tenantx:quota:"
	}
	if resolve == nil {
		return nil, fmt.Errorf("tenantx: QuotaResolver 不允许为空")
	}
	return &QuotaLimiter{store: store, keyPrefix: keyPrefix, resolve: resolve, periodSecs: 60}, nil
}

// QuotaDecision 配额判定结果。
type QuotaDecision struct {
	// Allowed 是否放行。
	Allowed bool
	// Limited 是否命中限流(Allowed=false 时必为 true)。
	Limited bool
	// Quota 生效配额(不限为 0)。
	Quota int
	// Remaining 分钟桶剩余配额(HitQuota 桶未满时 = quota-1 之后余量近似,仅提示用)。
	Remaining int
	// RetryAfter 超限时建议的重试等待秒数(分钟桶对齐剩余)。
	RetryAfter int
	// Headers 建议写出的 RateLimit 头。
	Headers map[string]string
}

// Allow 判定租户本次请求是否放行(不写响应,供调用方决定)。
func (q *QuotaLimiter) Allow(ctx context.Context, tenantID int64) (QuotaDecision, error) {
	quota := q.resolve(ctx, tenantID)
	if quota <= 0 { // default 租户与 quota=0 不限
		return QuotaDecision{Allowed: true}, nil
	}

	// PeriodLimit 是轻量结构,per-tenant 每请求构造即得,避免配额变更的缓存失效问题
	pl := limit.NewPeriodLimit(q.periodSecs, quota, q.store, fmt.Sprintf("%s%d:", q.keyPrefix, tenantID),
		limit.Align())
	state, err := pl.TakeCtx(ctx, "rpm")
	if err != nil {
		// Redis 故障:配额属增强能力,放行(fail-open),由调用方记录告警
		return QuotaDecision{Allowed: true}, err
	}

	decision := QuotaDecision{Quota: quota, Headers: map[string]string{
		HeaderRateLimitLimit: fmt.Sprintf("%d", quota),
	}}
	switch state {
	case limit.OverQuota:
		decision.Allowed = false
		decision.Limited = true
		decision.RetryAfter = secondsToWindowEnd(q.periodSecs)
		decision.Headers[HeaderRetryAfter] = fmt.Sprintf("%d", decision.RetryAfter)
	case limit.HitQuota:
		// 本请求恰好用完配额:放行,但提示余量为 0
		decision.Allowed = true
		decision.Remaining = 0
		decision.Headers[HeaderRateLimitRemaining] = "0"
	default: // limit.Allowed
		decision.Allowed = true
		decision.Remaining = quota - 1
		decision.Headers[HeaderRateLimitRemaining] = fmt.Sprintf("%d", decision.Remaining)
	}
	return decision, nil
}

// secondsToWindowEnd 分钟桶(对齐模式)距下一窗口的秒数。
func secondsToWindowEnd(periodSecs int) int {
	now := time.Now().Unix()
	rem := now % int64(periodSecs)
	return int(int64(periodSecs) - rem)
}

// WriteRateLimited 限流短路响应:429 + RateLimit 头 + {code,msg} 信封。
// 限流是系统行为,HTTP 429(区别于业务错误的 200),信封 code=110429 供前端统一提示。
func WriteRateLimited(w http.ResponseWriter, decision QuotaDecision) {
	body := errcode.ErrRateLimited.Body()
	for k, v := range decision.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(body)
}
