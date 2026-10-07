// Package tenantx 是多租户 SQL 规约的支撑包(ADR-08/16 配套)。
//
// v4 口径:租户过滤不在 ORM 层做隐式魔法,而是 **model 层 SQL 显式携带
// `tenant_id = ?`**(ADR-08),本包负责:
//   - ctx 租户读取:MustTenantFromCtx fail-closed(无租户上下文的业务查询直接拒绝);
//   - Skip 显式豁免(后台作业/平台任务),豁免必须可审计;
//   - Scoped/Exempt 登记表:租户表与平台共享表登记,tenantaudit CI 静态对账
//     (租户表 model SQL 必含 tenant_id 条件/列,INSERT 必含 tenant_id 列);
//   - PeriodLimit RPM 分钟桶配额:429 + RateLimit 头(default 租户与 quota=0 不限)。
package tenantx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/zxiaosi-micro/micro-common/ctxkit"
)

// ErrNoTenant 无租户上下文(fail-closed 拒绝)。
var ErrNoTenant = errors.New("tenantx: 上下文中没有租户(业务查询必须携带租户,后台作业用 tenantx.Skip 显式豁免)")

type skipKey struct{}

// Skip 显式豁免租户约束(后台作业/跨租户平台任务)。
// 豁免必须显式而非遗漏——After/Tenant 返回值都为空时业务查询会被拒绝。
func Skip(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipKey{}, true)
}

// Skipped 查询是否处于显式豁免状态。
func Skipped(ctx context.Context) bool {
	v, _ := ctx.Value(skipKey{}).(bool)
	return v
}

// TenantFromCtx 读租户(与 ctxkit 同键:authz/eventbus 注入一次,这里读取)。
func TenantFromCtx(ctx context.Context) (int64, bool) {
	return ctxkit.Tenant(ctx)
}

// WithTenant 注入租户(ctxkit 同键,事件消费/后台任务恢复租户上下文用)。
func WithTenant(ctx context.Context, tenantID int64) context.Context {
	return ctxkit.WithTenant(ctx, tenantID)
}

// MustTenantFromCtx fail-closed 读租户:
//   - 有租户 → (tid, nil);
//   - Skip 豁免 → (0, nil)——调用方据此构造跨租户 SQL(不拼 tenant_id 条件);
//   - 都没有 → ErrNoTenant,业务查询直接拒绝。
func MustTenantFromCtx(ctx context.Context) (int64, error) {
	if Skipped(ctx) {
		return 0, nil
	}
	if tid, ok := TenantFromCtx(ctx); ok && tid > 0 {
		return tid, nil
	}
	return 0, ErrNoTenant
}

// tenantx 经 ctxkit.WithTenant/Tenant 存取,保证与 authz 注入、eventbus 恢复同键同源。

// —— Scoped/Exempt 登记表(tenantaudit 对账依据) ——

// Kind 登记类型。
type Kind string

const (
	// KindScoped 租户表:model SQL 必须显式携带 tenant_id 条件/列,INSERT 必含 tenant_id。
	KindScoped Kind = "scoped"
	// KindExempt 平台共享表豁免(登记理由)。
	KindExempt Kind = "exempt"
)

// Registration 一条登记。
type Registration struct {
	Table  string `json:"table"`
	Kind   Kind   `json:"kind"`
	Reason string `json:"reason"`
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Registration{}
)

// Scoped 登记租户表(重复登记幂等;同表不同类型 panic)。
// 服务在 model 包 init 中登记,tenantaudit 据此对账。
func Scoped(table string) {
	register(Registration{Table: table, Kind: KindScoped, Reason: "租户表"})
}

// Exempt 登记平台共享表豁免(必须给理由)。
func Exempt(table, reason string) {
	if reason == "" {
		panic(fmt.Sprintf("tenantx: 表 %s 豁免登记必须说明理由", table))
	}
	register(Registration{Table: table, Kind: KindExempt, Reason: reason})
}

func register(r Registration) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if prev, ok := registry[r.Table]; ok {
		if prev.Kind != r.Kind {
			panic(fmt.Sprintf("tenantx: 表 %s 重复登记且类型冲突(%s vs %s)", r.Table, prev.Kind, r.Kind))
		}
		return
	}
	registry[r.Table] = r
}

// Registrations 返回全部登记(按表名排序,tenantaudit 对账输出)。
func Registrations() []Registration {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Registration, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}
