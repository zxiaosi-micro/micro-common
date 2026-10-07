package ctxkit

import (
	"context"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	ctx = WithUID(ctx, 12345)
	ctx = WithSID(ctx, "sess-abc")
	ctx = WithClient(ctx, ClientAdminWeb)
	ctx = WithRoles(ctx, []string{"admin", "ops"})
	ctx = WithTenant(ctx, 42)
	ctx = WithDataScope(ctx, `{"org_ids":[1,2]}`)

	if UID(ctx) != 12345 {
		t.Fatalf("UID = %d", UID(ctx))
	}
	if SID(ctx) != "sess-abc" {
		t.Fatalf("SID = %q", SID(ctx))
	}
	if Client(ctx) != ClientAdminWeb {
		t.Fatalf("Client = %q", Client(ctx))
	}
	roles := Roles(ctx)
	if len(roles) != 2 || roles[0] != "admin" {
		t.Fatalf("Roles = %v", roles)
	}
	tid, ok := Tenant(ctx)
	if !ok || tid != 42 {
		t.Fatalf("Tenant = (%d, %v)", tid, ok)
	}
	if DataScope(ctx) != `{"org_ids":[1,2]}` {
		t.Fatalf("DataScope = %q", DataScope(ctx))
	}
}

func TestAbsentZeroValues(t *testing.T) {
	ctx := context.Background()
	if UID(ctx) != 0 || SID(ctx) != "" || Client(ctx) != "" {
		t.Fatal("未注入应返回零值")
	}
	if Roles(ctx) != nil {
		t.Fatal("未注入 Roles 应为 nil")
	}
	if tid, ok := Tenant(ctx); ok || tid != 0 {
		t.Fatal("未注入 Tenant 应为 (0,false)")
	}
	if DataScope(ctx) != "" {
		t.Fatal("未注入 DataScope 应为空串")
	}
}

func TestKeysDoNotCollide(t *testing.T) {
	// 各键互不串扰:只注入一个值,其余取值保持零值
	ctx := WithClient(context.Background(), ClientOpsApp)
	if UID(ctx) != 0 || SID(ctx) != "" || DataScope(ctx) != "" {
		t.Fatal("ctx 键串扰")
	}
}

func TestChainPropagation(t *testing.T) {
	// 模拟全链透传:BFF 注入 → logic → RPC 客户端 ctx 派生仍可读
	base := WithUID(WithTenant(context.Background(), 7), 99)
	type otherKey struct{}
	child := context.WithValue(base, otherKey{}, "x")
	if UID(child) != 99 {
		t.Fatal("派生 ctx 丢失 uid")
	}
	if tid, ok := Tenant(child); !ok || tid != 7 {
		t.Fatal("派生 ctx 丢失租户")
	}
}
