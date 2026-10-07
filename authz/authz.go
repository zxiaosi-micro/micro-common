// Package authz 实现 BFF 三层鉴权中间件(02 §9.5,按 zero-skills middleware 模式):
//
//	验签(kid 选钥)→ 会话(sess:{sid})→ client 匹配 → RBAC 每请求 GET auth_cache
//	→ 敏感路由 auth_level(step-up)→ ctx 注入 → RPC metadata 桥(见 bridge.go)。
//
// 降级策略(NFR-AVL-005):Redis 故障时普通路由 fail-open
// (本地 collection.Cache LRU 兜底 + DegradeHook 告警,暴露窗口 ≤ access 剩余 30min);
// FailClose 路径(指令/财务)直接拒绝。
//
// 中间件通过 httpx.ErrorCtx 写出错误——服务必须先 response.Setup() 注册全局错误出口。
package authz

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/collection"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest/httpx"

	"github.com/zxiaosi-micro/micro-common/ctxkit"
	"github.com/zxiaosi-micro/micro-common/errcode"
	"github.com/zxiaosi-micro/micro-common/jwtauth"
	"github.com/zxiaosi-micro/micro-common/sessionx"
)

// AuthSnapshot 权限快照(存储与类型由 sessionx 拥有,authz 别名导出)。
type AuthSnapshot = sessionx.AuthSnapshot

// SessionStore 会话校验接口(*sessionx.Store 满足;接口化便于单测与降级模拟)。
type SessionStore interface {
	// Get 读会话;不存在返回 sessionx.ErrSessionNotFound。
	Get(ctx context.Context, sid string) (*sessionx.Session, error)
	// TombReason 查会话注销原因(区分被顶 10402 / 注销 10403)。
	TombReason(ctx context.Context, sid string) (string, error)
}

// AuthCache auth_cache 读取接口(*sessionx.Store 满足;identity 是唯一写入方)。
type AuthCache interface {
	// GetAuthCache 读 uid 的权限快照;无记录返回 sessionx.ErrAuthCacheMiss。
	GetAuthCache(ctx context.Context, uid int64) (*AuthSnapshot, error)
}

// PermResolver 路由→权限码解析器;返回 ok=false 表示该路由仅需登录。
// 由 BFF 按路由表注册(goctl 生成路由的 handler 归组对接)。
type PermResolver func(r *http.Request) (permCode string, ok bool)

// Conf 中间件配置(BFF yaml 注入)。
type Conf struct {
	// SkipPaths 免鉴权路径前缀(/auth/* /sso/* /healthz)。
	SkipPaths []string `json:",optional"`
	// FailClosePrefixes fail-close 路径前缀(指令下行/财务);Redis 故障时拒绝而非放行。
	FailClosePrefixes []string `json:",optional"`
	// SensitivePrefixes 敏感路由前缀:要求 auth_level ≥ 2(step-up)。
	SensitivePrefixes []string `json:",optional"`
	// AllowedClients 本 BFF 允许的登录端;空 = 不校验。
	AllowedClients []string `json:",optional"`
	// CacheTTL LRU 兜底缓存 TTL(默认 30min,与 access TTL 同口径)。
	CacheTTL time.Duration `json:",optional"`
	// CacheLimit LRU 容量(默认 4096)。
	CacheLimit int `json:",optional"`
}

// Middleware BFF 鉴权中间件(rest.Middleware 形态:func(http.HandlerFunc) http.HandlerFunc)。
type Middleware struct {
	verifier *jwtauth.Verifier
	sessions SessionStore
	cache    AuthCache
	permOf   PermResolver
	lru      *collection.Cache
	conf     Conf
	degrade  func(reason string)
}

// Option 中间件选项。
type Option func(*Middleware)

// WithDegradeHook 降级告警钩子(Redis 故障 fail-open 时触发;服务接 logx 告警/指标)。
func WithDegradeHook(fn func(reason string)) Option {
	return func(m *Middleware) { m.degrade = fn }
}

// WithPermResolver 注册路由→权限码解析。
func WithPermResolver(fn PermResolver) Option {
	return func(m *Middleware) { m.permOf = fn }
}

// New 构造中间件。
func New(verifier *jwtauth.Verifier, sessions SessionStore, cache AuthCache, conf Conf, opts ...Option) (*Middleware, error) {
	ttl := conf.CacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	limitN := conf.CacheLimit
	if limitN <= 0 {
		limitN = 4096
	}
	lru, err := collection.NewCache(ttl, collection.WithLimit(limitN))
	if err != nil {
		return nil, err
	}
	m := &Middleware{verifier: verifier, sessions: sessions, cache: cache, lru: lru, conf: conf}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

func (m *Middleware) warnDegrade(reason string) {
	if m.degrade != nil {
		m.degrade(reason)
	}
	logx.Errorf("authz 降级: %s", reason)
}

// Handle 返回 http 中间件。
func (m *Middleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if matchPrefix(m.conf.SkipPaths, r.URL.Path) {
			next(w, r)
			return
		}
		failClose := matchPrefix(m.conf.FailClosePrefixes, r.URL.Path)

		// ① 验签(kid 选钥)
		claims, err := m.authenticate(r)
		if err != nil {
			httpx.ErrorCtx(ctx, w, err)
			return
		}

		// ② 会话校验(sess:{sid},服务端权威;被顶→10402,注销→10403)
		if err := m.checkSession(ctx, claims.SID, r.URL.Path); err != nil {
			httpx.ErrorCtx(ctx, w, err)
			return
		}

		// ③ client 匹配(防"小程序 token 调管理端接口")
		if !clientAllowed(m.conf.AllowedClients, claims.Client) {
			httpx.ErrorCtx(ctx, w, errcode.ErrTokenInvalid)
			return
		}

		// ④ RBAC:每请求一次 GET auth_cache(Redis 故障走 LRU 兜底/fail-open)
		snapshot, cacheErr := m.cache.GetAuthCache(ctx, claims.UID)
		switch {
		case cacheErr == nil:
			m.lru.Set(cacheKey(claims.UID), snapshot)
		case errors.Is(cacheErr, sessionx.ErrAuthCacheMiss):
			// 无记录 ≠ 故障:视为无权限数据,需权限的路由将拒绝
			snapshot = nil
		default:
			// Redis 故障:fail-close 路径(指令/财务)一律拒绝,不用兜底数据
			if failClose {
				httpx.ErrorCtx(ctx, w, errcode.ErrServiceUnavailable.WithMsg("鉴权服务暂不可用"))
				return
			}
			// 普通路由:本地 LRU 兜底
			if v, ok := m.lru.Get(cacheKey(claims.UID)); ok {
				snapshot = v.(*AuthSnapshot)
				m.warnDegrade("auth_cache Redis 故障,LRU 兜底 uid=" + strconv.FormatInt(claims.UID, 10))
			} else {
				// fail-open:放行,暴露窗口 ≤ access 剩余 30min + 告警
				m.warnDegrade("auth_cache Redis 故障且无本地兜底,fail-open uid=" + strconv.FormatInt(claims.UID, 10))
			}
		}

		// 路由权限码(RBAC 判定)
		if m.permOf != nil {
			if perm, ok := m.permOf(r); ok && !hasPerm(snapshot, perm) {
				httpx.ErrorCtx(ctx, w, errcode.ErrPermissionDenied)
				return
			}
		}

		// ⑤ 敏感路由 auth_level(step-up)
		if matchPrefix(m.conf.SensitivePrefixes, r.URL.Path) && claims.Level < 2 {
			httpx.ErrorCtx(ctx, w, errcode.ErrStepUpRequired)
			return
		}

		// ⑥ ctx 注入(uid/sid/client/roles/tenant/data_scope 全链透传)
		rctx := ctxkit.WithUID(ctx, claims.UID)
		rctx = ctxkit.WithSID(rctx, claims.SID)
		rctx = ctxkit.WithClient(rctx, claims.Client)
		if snapshot != nil {
			rctx = ctxkit.WithRoles(rctx, snapshot.Roles)
			rctx = ctxkit.WithTenant(rctx, snapshot.TenantID)
			rctx = ctxkit.WithDataScope(rctx, snapshot.DataScope)
		}
		next(w, r.WithContext(rctx))
	}
}

func (m *Middleware) authenticate(r *http.Request) (*jwtauth.Claims, error) {
	auth := r.Header.Get("Authorization")
	if len(auth) < 8 || !strings.EqualFold(auth[:7], "Bearer ") {
		return nil, errcode.ErrTokenInvalid
	}
	token := strings.TrimSpace(auth[7:])
	if token == "" {
		return nil, errcode.ErrTokenInvalid
	}
	return m.verifier.Verify(token)
}

func (m *Middleware) checkSession(ctx context.Context, sid, path string) error {
	_, err := m.sessions.Get(ctx, sid)
	if err == nil {
		return nil
	}
	if errors.Is(err, sessionx.ErrSessionNotFound) {
		// 会话不存在:查墓碑区分被顶(10402 互斥弹窗)/注销(10403)
		if reason, terr := m.sessions.TombReason(ctx, sid); terr == nil {
			if reason == sessionx.ReasonKicked {
				return errcode.ErrSessionKicked
			}
			return errcode.ErrSessionRevoked
		}
		return errcode.ErrSessionRevoked
	}
	// Redis 故障:会话校验不可得。普通路由按 fail-open 继续走(会话层放行,
	// 权限层仍有 LRU 兜底),fail-close 路径拒绝。
	if matchPrefix(m.conf.FailClosePrefixes, path) {
		return errcode.ErrServiceUnavailable.WithMsg("鉴权服务暂不可用")
	}
	m.warnDegrade("会话校验 Redis 故障,fail-open sid=" + sid)
	return nil
}

func hasPerm(s *AuthSnapshot, perm string) bool {
	if s == nil {
		return false
	}
	return s.HasPerm(perm)
}

func clientAllowed(allowed []string, client string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, c := range allowed {
		if c == client {
			return true
		}
	}
	return false
}

func matchPrefix(prefixes []string, path string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func cacheKey(uid int64) string { return "authz:" + strconv.FormatInt(uid, 10) }
