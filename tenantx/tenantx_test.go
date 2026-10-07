package tenantx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"

	"github.com/zxiaosi-micro/micro-common/ctxkit"
	"github.com/zxiaosi-micro/micro-common/errcode"
	"github.com/zxiaosi-micro/micro-common/testinfra"
)

// —— ctx 租户 fail-closed ——

func TestMustTenantFromCtx(t *testing.T) {
	// 有租户
	ctx := ctxkit.WithTenant(context.Background(), 42)
	tid, err := MustTenantFromCtx(ctx)
	if err != nil || tid != 42 {
		t.Fatalf("取租户失败: %d err=%v", tid, err)
	}
	// 无租户 → fail-closed
	if _, err := MustTenantFromCtx(context.Background()); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("无租户应返回 ErrNoTenant: %v", err)
	}
	// Skip 豁免 → (0, nil)
	tid, err = MustTenantFromCtx(Skip(context.Background()))
	if err != nil || tid != 0 {
		t.Fatalf("Skip 豁免应返回 (0,nil): %d err=%v", tid, err)
	}
	// 与 ctxkit 同键
	if tid, ok := TenantFromCtx(ctx); !ok || tid != 42 {
		t.Fatal("TenantFromCtx 应与 ctxkit 同键")
	}
	if tid, ok := ctxkit.Tenant(WithTenant(context.Background(), 7)); !ok || tid != 7 {
		t.Fatal("WithTenant 应与 ctxkit 同键")
	}
}

func TestSkipped(t *testing.T) {
	if Skipped(context.Background()) {
		t.Fatal("默认不豁免")
	}
	if !Skipped(Skip(context.Background())) {
		t.Fatal("Skip 后应豁免")
	}
}

// —— 登记表 ——

func TestRegistry(t *testing.T) {
	Scoped("order")            // 服务侧样例
	Scoped("order")            // 幂等
	Exempt("product", "平台共享表") // 登记理由
	registrations := Registrations()
	var foundOrder, foundProduct bool
	for _, r := range registrations {
		if r.Table == "order" && r.Kind == KindScoped {
			foundOrder = true
		}
		if r.Table == "product" && r.Kind == KindExempt && r.Reason == "平台共享表" {
			foundProduct = true
		}
	}
	if !foundOrder || !foundProduct {
		t.Fatalf("登记缺失: %+v", registrations)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("同表不同类型登记应 panic")
		}
	}()
	Scoped("order")
	Exempt("order", "冲突")
}

func TestExemptRequiresReason(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("无理由豁免应 panic")
		}
	}()
	Exempt("no-reason-table", "")
}

// —— 配额 ——

func newQuotaLimiter(t *testing.T, resolve QuotaResolver) *QuotaLimiter {
	t.Helper()
	conn, _ := testinfra.Redis(t)
	store := redis.MustNewRedis(redis.RedisConf{Host: conn.Addr, Pass: conn.Password, Type: "node"})
	// 每次执行用独立前缀,规避跨次运行残留计数(无 flush 权限时仍可重复跑)
	prefix := fmt.Sprintf("microtest:quota:%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		// 桶 key 带 60s TTL,靠过期自动清理;这里主动删一次加快回收
		keys, err := store.Keys(prefix + "*")
		if err == nil && len(keys) > 0 {
			_, _ = store.Del(keys...)
		}
	})
	q, err := NewQuotaLimiter(store, prefix, resolve)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQuotaAllowUnlimited(t *testing.T) {
	// quota=0 → 不限
	q := newQuotaLimiter(t, func(context.Context, int64) int { return 0 })
	for i := 0; i < 5; i++ {
		d, err := q.Allow(context.Background(), 9001)
		if err != nil || !d.Allowed || d.Limited || d.Quota != 0 {
			t.Fatalf("不限流应放行: %+v err=%v", d, err)
		}
	}
}

func TestQuotaLimitAndHeaders(t *testing.T) {
	const quota = 3
	q := newQuotaLimiter(t, func(context.Context, int64) int { return quota })
	ctx := context.Background()

	// 前 quota 次:放行并带 Limit/Remaining 头
	for i := 0; i < quota; i++ {
		d, err := q.Allow(ctx, 9002)
		if err != nil || !d.Allowed {
			t.Fatalf("第 %d 次应放行: %+v err=%v", i+1, d, err)
		}
		if d.Headers[HeaderRateLimitLimit] != "3" {
			t.Fatalf("Limit 头不符: %v", d.Headers)
		}
	}
	// 第 4 次:429 语义 + Retry-After
	d, err := q.Allow(ctx, 9002)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || !d.Limited {
		t.Fatalf("超限应拒绝: %+v", d)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > 60 {
		t.Fatalf("RetryAfter = %d", d.RetryAfter)
	}
	if _, ok := d.Headers[HeaderRetryAfter]; !ok {
		t.Fatal("应带 Retry-After 头")
	}
	// 不同租户互不影响
	if d, err := q.Allow(ctx, 9003); err != nil || !d.Allowed {
		t.Fatalf("另一租户不应受影响: %+v err=%v", d, err)
	}
}

func TestWriteRateLimited(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteRateLimited(rec, QuotaDecision{
		Limited:    true,
		Headers:    map[string]string{HeaderRetryAfter: "30", HeaderRateLimitLimit: "10"},
		RetryAfter: 30,
	})
	if rec.Code != 429 {
		t.Fatalf("应 429: %d", rec.Code)
	}
	if rec.Header().Get(HeaderRetryAfter) != "30" || rec.Header().Get(HeaderRateLimitLimit) != "10" {
		t.Fatalf("响应头不符: %v", rec.Header())
	}
	var body errcode.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != errcode.CodeRateLimited {
		t.Fatalf("信封应为 110429: %s", rec.Body.String())
	}
}
