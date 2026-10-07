package authz

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/zxiaosi-micro/micro-common/ctxkit"
	"github.com/zxiaosi-micro/micro-common/errcode"
	"github.com/zxiaosi-micro/micro-common/jwtauth"
	"github.com/zxiaosi-micro/micro-common/response"
	"github.com/zxiaosi-micro/micro-common/sessionx"
)

// —— 测试替身 ——

type fakeSessions struct {
	get  func(ctx context.Context, sid string) (*sessionx.Session, error)
	tomb map[string]string
}

func (f *fakeSessions) Get(ctx context.Context, sid string) (*sessionx.Session, error) {
	return f.get(ctx, sid)
}

func (f *fakeSessions) TombReason(ctx context.Context, sid string) (string, error) {
	if f.tomb == nil {
		return "", nil
	}
	return f.tomb[sid], nil
}

type fakeCache struct {
	get func(ctx context.Context, uid int64) (*AuthSnapshot, error)
}

func (f *fakeCache) GetAuthCache(ctx context.Context, uid int64) (*AuthSnapshot, error) {
	return f.get(ctx, uid)
}

var (
	errRedisDown = errors.New("redis: connection refused")
	testSnapshot = &AuthSnapshot{
		Roles:     []string{"admin"},
		Perms:     []string{"order:create", "order:view"},
		DataScope: `{"org_ids":[1]}`,
		TenantID:  42,
	}
)

func okSessions() *fakeSessions {
	return &fakeSessions{get: func(context.Context, string) (*sessionx.Session, error) {
		return &sessionx.Session{SID: "s", UID: 1001, Client: "ADMIN_WEB"}, nil
	}}
}

func okCache() *fakeCache {
	return &fakeCache{get: func(context.Context, int64) (*AuthSnapshot, error) {
		return testSnapshot, nil
	}}
}

// —— 装置 ——

type fixture struct {
	mw       *Middleware
	signer   *jwtauth.Signer
	sessions *fakeSessions
	cache    *fakeCache
	degrades []string
}

func newFixture(t *testing.T, conf Conf) *fixture {
	t.Helper()
	response.Setup() // 全局错误出口(中间件经 httpx.ErrorCtx 写出)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jwtauth.NewSigner("test-kid", key)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := jwtauth.NewVerifier(map[string]*rsa.PublicKey{"test-kid": &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{signer: signer, sessions: okSessions(), cache: okCache()}
	mw, err := New(verifier, f.sessions, f.cache, conf, WithDegradeHook(func(reason string) {
		f.degrades = append(f.degrades, reason)
	}), WithPermResolver(func(r *http.Request) (string, bool) {
		if r.URL.Path == "/api/v1/orders" && r.Method == http.MethodPost {
			return "order:delete", true
		}
		return "", false
	}))
	if err != nil {
		t.Fatal(err)
	}
	f.mw = mw
	return f
}

func (f *fixture) token(t *testing.T, uid int64, sid, client string, level int) string {
	t.Helper()
	tok, err := f.signer.Sign(jwtauth.Claims{UID: uid, SID: sid, Client: client, Level: level}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *fixture) serve(t *testing.T, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	var gotUID int64
	f.mw.Handle(func(w http.ResponseWriter, r *http.Request) {
		gotUID = ctxkit.UID(r.Context())
		w.WriteHeader(http.StatusOK)
	})(rec, req)
	t.Logf("path=%s status=%d body=%s ctxUID=%d", path, rec.Code, rec.Body.String(), gotUID)
	return rec
}

func bodyCode(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	var b errcode.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("响应体非信封: %s", rec.Body.String())
	}
	return b.Code
}

// —— 用例 ——

func TestSkipPath(t *testing.T) {
	f := newFixture(t, Conf{SkipPaths: []string{"/auth/"}})
	rec := f.serve(t, "", "/auth/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("免鉴权路径应放行: %d", rec.Code)
	}
}

func TestMissingTokenRejected(t *testing.T) {
	f := newFixture(t, Conf{})
	rec := f.serve(t, "", "/api/v1/me")
	if rec.Code != http.StatusUnauthorized || bodyCode(t, rec) != errcode.CodeTokenInvalid {
		t.Fatalf("缺 token 应 401/10401: %d %s", rec.Code, rec.Body.String())
	}
}

func TestFullChainCtxInjection(t *testing.T) {
	f := newFixture(t, Conf{AllowedClients: []string{"ADMIN_WEB"}})
	tok := f.token(t, 1001, "sess-1", "ADMIN_WEB", 1)

	var uid, tenant int64
	var sid, client string
	var roles []string
	var ds string
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	f.mw.Handle(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		uid, sid, client = ctxkit.UID(ctx), ctxkit.SID(ctx), ctxkit.Client(ctx)
		tenant, _ = ctxkit.Tenant(ctx)
		roles = ctxkit.Roles(ctx)
		ds = ctxkit.DataScope(ctx)
		w.WriteHeader(http.StatusOK)
	})(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应放行: %d %s", rec.Code, rec.Body.String())
	}
	if uid != 1001 || sid != "sess-1" || client != "ADMIN_WEB" {
		t.Fatalf("ctx 注入不符: uid=%d sid=%s client=%s", uid, sid, client)
	}
	if tenant != 42 || len(roles) != 1 || roles[0] != "admin" || ds != `{"org_ids":[1]}` {
		t.Fatalf("auth_cache 注入不符: tenant=%d roles=%v ds=%s", tenant, roles, ds)
	}
}

func TestKickedVsRevoked(t *testing.T) {
	// 被顶(墓碑=kicked)→ 10402
	f := newFixture(t, Conf{})
	tok := f.token(t, 1001, "sess-kicked", "ADMIN_WEB", 1)
	f.sessions = &fakeSessions{
		get: func(context.Context, string) (*sessionx.Session, error) {
			return nil, sessionx.ErrSessionNotFound
		},
		tomb: map[string]string{"sess-kicked": sessionx.ReasonKicked},
	}
	mw, err := New(f.mw.verifier, f.sessions, f.cache, Conf{})
	if err != nil {
		t.Fatal(err)
	}
	f.mw = mw
	rec := f.serve(t, tok, "/api/v1/me")
	if rec.Code != http.StatusUnauthorized || bodyCode(t, rec) != errcode.CodeSessionKicked {
		t.Fatalf("被顶应 10402: %d %s", rec.Code, rec.Body.String())
	}

	// 注销(无墓碑)→ 10403
	f.sessions = &fakeSessions{get: func(context.Context, string) (*sessionx.Session, error) {
		return nil, sessionx.ErrSessionNotFound
	}}
	mw, _ = New(f.mw.verifier, f.sessions, f.cache, Conf{})
	f.mw = mw
	rec = f.serve(t, tok, "/api/v1/me")
	if bodyCode(t, rec) != errcode.CodeSessionRevoked {
		t.Fatalf("注销应 10403: %s", rec.Body.String())
	}
}

func TestClientMismatch(t *testing.T) {
	f := newFixture(t, Conf{AllowedClients: []string{"ADMIN_WEB"}})
	tok := f.token(t, 1001, "sess-1", "CLIENT_MINI", 1) // 小程序 token 调管理端
	rec := f.serve(t, tok, "/api/v1/me")
	if bodyCode(t, rec) != errcode.CodeTokenInvalid {
		t.Fatalf("client 不匹配应拒绝: %s", rec.Body.String())
	}
}

func TestRBACPermissionDenied(t *testing.T) {
	f := newFixture(t, Conf{})
	tok := f.token(t, 1001, "sess-1", "ADMIN_WEB", 1)
	rec := f.serve(t, tok, "/api/v1/orders") // 需要 order:delete(快照未持有)
	if rec.Code != http.StatusForbidden || bodyCode(t, rec) != errcode.CodePermissionDenied {
		t.Fatalf("无权限应 403/10408: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSensitiveRequiresStepUp(t *testing.T) {
	f := newFixture(t, Conf{SensitivePrefixes: []string{"/api/v1/cmd"}})
	tok := f.token(t, 1001, "sess-1", "ADMIN_WEB", 1)
	rec := f.serve(t, tok, "/api/v1/cmd/send")
	if bodyCode(t, rec) != errcode.CodeStepUpRequired {
		t.Fatalf("敏感路由应要求提级 10406: %s", rec.Body.String())
	}
	// step-up 后(level=2)放行
	tok2 := f.token(t, 1001, "sess-1", "ADMIN_WEB", 2)
	rec = f.serve(t, tok2, "/api/v1/cmd/send")
	if rec.Code != http.StatusOK {
		t.Fatalf("提级后应放行: %d %s", rec.Code, rec.Body.String())
	}
}

// 验收口径:Redis 故障 → 普通路由 fail-open(LRU 兜底 + 告警),FailClose 拒绝。
func TestDegradeFailOpenAndFailClose(t *testing.T) {
	conf := Conf{FailClosePrefixes: []string{"/api/v1/fin/"}}
	f := newFixture(t, conf)
	tok := f.token(t, 1001, "sess-1", "ADMIN_WEB", 1)

	// 正常一次:LRU 养缓存
	rec := f.serve(t, tok, "/api/v1/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("正常路径应放行: %s", rec.Body.String())
	}

	// Redis 故障:普通路由 fail-open(LRU 兜底)
	f.cache.get = func(context.Context, int64) (*AuthSnapshot, error) { return nil, errRedisDown }
	rec = f.serve(t, tok, "/api/v1/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("Redis 故障普通路由应 fail-open: %d %s", rec.Code, rec.Body.String())
	}
	if len(f.degrades) == 0 {
		t.Fatal("应触发降级告警钩子")
	}

	// 无 LRU 兜底:普通路由仍放行(完全 fail-open)
	f2 := newFixture(t, conf)
	f2.cache.get = func(context.Context, int64) (*AuthSnapshot, error) { return nil, errRedisDown }
	tok2 := f2.token(t, 1001, "sess-1", "ADMIN_WEB", 1)
	rec = f2.serve(t, tok2, "/api/v1/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("无兜底普通路由应 fail-open: %d %s", rec.Code, rec.Body.String())
	}

	// Redis 故障:fail-close 路径(指令/财务)拒绝
	f.cache.get = func(context.Context, int64) (*AuthSnapshot, error) { return nil, errRedisDown }
	rec = f.serve(t, tok, "/api/v1/fin/settle")
	if bodyCode(t, rec) != errcode.CodeServiceUnavailable {
		t.Fatalf("fail-close 路径应拒绝: %s", rec.Body.String())
	}
}

// —— metadata 桥 ——

func TestBridgeRoundTrip(t *testing.T) {
	// BFF 侧 ctx → OutgoingInterceptor → metadata
	base := ctxkit.WithUID(context.Background(), 1001)
	base = ctxkit.WithSID(base, "sess-9")
	base = ctxkit.WithClient(base, ctxkit.ClientAdminWeb)
	base = ctxkit.WithRoles(base, []string{"admin", "ops"})
	base = ctxkit.WithTenant(base, 42)
	base = ctxkit.WithDataScope(base, `{"org_ids":[1,2]}`)

	var capturedMD metadata.MD
	var innerCtx context.Context
	err := OutgoingInterceptor(base, "/pkg.Svc/Get", nil, nil, nil,
		func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			innerCtx = ctx
			md, _ := metadata.FromOutgoingContext(ctx)
			capturedMD = md
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if capturedMD == nil {
		t.Fatal("应注入 metadata")
	}
	if got := capturedMD.Get(MetaUID); len(got) != 1 || got[0] != "1001" {
		t.Fatalf("MetaUID = %v", got)
	}
	if got := capturedMD.Get(MetaRoles); got[0] != "admin,ops" {
		t.Fatalf("MetaRoles = %v", got)
	}
	if got := capturedMD.Get(MetaTenant); got[0] != "42" {
		t.Fatalf("MetaTenant = %v", got)
	}
	_ = innerCtx

	// metadata → ServerInterceptor → ctxkit 值还原
	rpcCtx := metadata.NewIncomingContext(context.Background(), capturedMD)
	_, err = ServerInterceptor(rpcCtx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
		if ctxkit.UID(ctx) != 1001 || ctxkit.SID(ctx) != "sess-9" {
			t.Fatalf("uid/sid 还原失败: %d/%s", ctxkit.UID(ctx), ctxkit.SID(ctx))
		}
		if ctxkit.Client(ctx) != ctxkit.ClientAdminWeb || len(ctxkit.Roles(ctx)) != 2 {
			t.Fatalf("client/roles 还原失败")
		}
		if tid, ok := ctxkit.Tenant(ctx); !ok || tid != 42 {
			t.Fatalf("tenant 还原失败: %d", tid)
		}
		if ctxkit.DataScope(ctx) != `{"org_ids":[1,2]}` {
			t.Fatalf("data_scope 还原失败")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBridgeErrorCodeRoundTrip(t *testing.T) {
	// 服务端 logic 返回 CodeError → ServerInterceptor 转 gRPC status
	_, err := ServerInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{},
		func(ctx context.Context, req any) (any, error) {
			return nil, errcode.New(errcode.SegInventory, 23, "库存不足")
		})
	// 客户端 OutgoingInterceptor 把 status 还原为 CodeError
	ce, ok := errcode.FromGRPC(err)
	if !ok || ce.Code() != 40023 {
		t.Fatalf("业务码桥接失败: %v", err)
	}
}
