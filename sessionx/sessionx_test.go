package sessionx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zxiaosi-micro/micro-common/testinfra"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	conn, _ := testinfra.Redis(t)
	rdb := redis.NewClient(&redis.Options{Addr: conn.Addr, Password: conn.Password, DB: 4})
	t.Cleanup(func() { _ = rdb.FlushDB(context.Background()); _ = rdb.Close() })
	return NewFromClient(rdb, Conf{
		Addr:       conn.Addr,
		DB:         4,
		SessionTTL: time.Hour,
		RefreshTTL: time.Hour,
		StepUpTTL:  200 * time.Millisecond, // 测试缩短
	})
}

func TestCreateAndGet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sess, kicked, err := s.Create(ctx, 1001, "ADMIN_WEB")
	if err != nil || len(kicked) != 0 {
		t.Fatalf("创建失败: %v kicked=%v", err, kicked)
	}
	got, err := s.Get(ctx, sess.SID)
	if err != nil || got.UID != 1001 || got.Client != "ADMIN_WEB" {
		t.Fatalf("读取会话不符: %+v err=%v", got, err)
	}
	// 不同 client 共存(QQ 模式跨端共存)
	if _, _, err := s.Create(ctx, 1001, "OPS_APP"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListByUID(ctx, 1001)
	if err != nil || len(list) != 2 {
		t.Fatalf("跨端应共存 2 个会话: %d err=%v", len(list), err)
	}
}

// 验收口径:同端互斥踢旧。
func TestMutexKicksOldSession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	s1, _, err := s.Create(ctx, 2001, "CLIENT_MINI")
	if err != nil {
		t.Fatal(err)
	}
	s2, kicked, err := s.Create(ctx, 2001, "CLIENT_MINI")
	if err != nil {
		t.Fatal(err)
	}
	if len(kicked) != 1 || kicked[0] != s1.SID {
		t.Fatalf("应踢旧会话 %s: %v", s1.SID, kicked)
	}
	// 旧会话已不可读,墓碑为 kicked
	if _, err := s.Get(ctx, s1.SID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("旧会话应已删除: %v", err)
	}
	if reason, _ := s.TombReason(ctx, s1.SID); reason != ReasonKicked {
		t.Fatalf("墓碑 = %q, want kicked", reason)
	}
	// 新会话可用
	if got, _ := s.Get(ctx, s2.SID); got == nil || got.SID != s2.SID {
		t.Fatal("新会话应可用")
	}
	// mutex 只指向最新
	if v, _ := s.rdb.Get(ctx, s.key("mutex:", "2001", ":", "CLIENT_MINI")).Result(); v != s2.SID {
		t.Fatalf("mutex = %q", v)
	}
}

func TestLogoutTombstone(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sess, _, _ := s.Create(ctx, 3001, "ADMIN_WEB")

	if err := s.Revoke(ctx, sess.SID, ReasonLogout); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, sess.SID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("登出后会话应删除")
	}
	if reason, _ := s.TombReason(ctx, sess.SID); reason != ReasonLogout {
		t.Fatalf("墓碑 = %q", reason)
	}
	// 幂等:重复登出不报错
	if err := s.Revoke(ctx, sess.SID, ReasonLogout); err != nil {
		t.Fatal(err)
	}
}

// 验收口径:refresh 重用触发全端注销。
func TestRefreshReuseRevokesAll(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sessA, _, _ := s.Create(ctx, 4001, "ADMIN_WEB")
	sessB, _, _ := s.Create(ctx, 4001, "OPS_APP")
	rtid, err := s.IssueRefresh(ctx, 4001, sessA.SID)
	if err != nil {
		t.Fatal(err)
	}

	// 正常轮换:换发新 rtid
	newRTID, gotSID, err := s.RotateRefresh(ctx, rtid)
	if err != nil || gotSID != sessA.SID || newRTID == "" {
		t.Fatalf("轮换失败: rtid=%s sid=%s err=%v", newRTID, gotSID, err)
	}
	// 会话本体仍在(轮换 = 滑动续期)
	if _, err := s.Get(ctx, sessA.SID); err != nil {
		t.Fatalf("轮换后会话应存活: %v", err)
	}

	// 重用旧 rtid → 全端注销
	_, _, err = s.RotateRefresh(ctx, rtid)
	if !errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("重用应返回 ErrRefreshReuse: %v", err)
	}
	// 全端(A/B 两个端)会话都被注销
	if _, err := s.Get(ctx, sessA.SID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("重用后 A 端会话应被注销")
	}
	if _, err := s.Get(ctx, sessB.SID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("重用后 B 端会话应被注销")
	}
	if reason, _ := s.TombReason(ctx, sessB.SID); reason != ReasonRevoked {
		t.Fatalf("全端注销墓碑 = %q", reason)
	}
}

func TestRefreshInvalid(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, _, err := s.RotateRefresh(ctx, "nonexistent"); !errors.Is(err, ErrRefreshInvalid) {
		t.Fatalf("无效 rtid 应返回 ErrRefreshInvalid: %v", err)
	}
}

// 验收口径:login_fail 锁定。
func TestLoginFailLockout(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ident := "hash:mobile-abc"

	// 前 4 次:未锁定
	for i := 0; i < 4; i++ {
		res, err := s.RecordLoginFail(ctx, ident, 5, time.Minute, 300*time.Millisecond)
		if err != nil || res.Locked {
			t.Fatalf("第 %d 次不应锁定: %+v err=%v", i+1, res, err)
		}
	}
	// 第 5 次:触发锁定
	res, err := s.RecordLoginFail(ctx, ident, 5, time.Minute, 300*time.Millisecond)
	if err != nil || !res.Locked || res.Remaining != 0 {
		t.Fatalf("第 5 次应锁定: %+v err=%v", res, err)
	}
	state, err := s.CheckLocked(ctx, ident)
	if err != nil || !state.Locked || state.Remaining <= 0 {
		t.Fatalf("应处于锁定状态: %+v err=%v", state, err)
	}
	// 到期自动解锁
	time.Sleep(350 * time.Millisecond)
	state, err = s.CheckLocked(ctx, ident)
	if err != nil || state.Locked {
		t.Fatalf("到期应解锁: %+v err=%v", state, err)
	}
	// 成功登录清零
	if _, err := s.RecordLoginFail(ctx, ident, 5, time.Minute, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearLoginFails(ctx, ident); err != nil {
		t.Fatal(err)
	}
	state, _ = s.CheckLocked(ctx, ident)
	if state.Locked {
		t.Fatal("清零后不应锁定")
	}
}

func TestRevokeClientAndAll(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, _, _ := s.Create(ctx, 5001, "ADMIN_WEB")
	b, _, _ := s.Create(ctx, 5001, "OPS_APP")
	c, _, _ := s.Create(ctx, 5001, "OPS_APP")

	n, err := s.RevokeClient(ctx, 5001, "OPS_APP")
	if err != nil || n != 2 {
		t.Fatalf("应踢 OPS_APP 2 个会话: %d err=%v", n, err)
	}
	if _, err := s.Get(ctx, a.SID); err != nil {
		t.Fatal("ADMIN_WEB 会话应保留")
	}
	if _, err := s.Get(ctx, b.SID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("OPS_APP 会话应被踢")
	}
	n, err = s.RevokeAll(ctx, 5001)
	if err != nil || n != 1 {
		t.Fatalf("全端注销应剩 1 个: %d err=%v", n, err)
	}
	if _, err := s.Get(ctx, c.SID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("全端注销后不应有存活会话")
	}
}

func TestStepUpLevel(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sess, _, _ := s.Create(ctx, 6001, "ADMIN_WEB")

	level, err := s.EffectiveLevel(ctx, sess.SID)
	if err != nil || level != 1 {
		t.Fatalf("默认级别应为 1: %d err=%v", level, err)
	}
	if err := s.RaiseLevel(ctx, sess.SID, 2); err != nil {
		t.Fatal(err)
	}
	level, _ = s.EffectiveLevel(ctx, sess.SID)
	if level != 2 {
		t.Fatalf("step-up 后应为 2: %d", level)
	}
	// 窗口过期回落
	time.Sleep(250 * time.Millisecond)
	level, _ = s.EffectiveLevel(ctx, sess.SID)
	if level != 1 {
		t.Fatalf("step-up 窗口过期应回落 1: %d", level)
	}
}
