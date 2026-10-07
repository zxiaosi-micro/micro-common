package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"github.com/zxiaosi-micro/micro-common/tenantx"
	"github.com/zxiaosi-micro/micro-common/testinfra"
)

// fakeSender 内存投递器(Sender 标准实现,替代 Kafka;传输层正确性由 kq 封装保证)。
type fakeSender struct {
	mu       sync.Mutex
	sent     []sentMsg
	failNext int // >0 时接下来 N 次 Send 返回错误
}

type sentMsg struct {
	topic, key, value string
}

func (f *fakeSender) Send(_ context.Context, topic, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errors.New("kafka not ready")
	}
	f.sent = append(f.sent, sentMsg{topic, key, value})
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeSender) all() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

// busFixture MySQL(testinfra 三级解析)+ 四表清空。
type busFixture struct {
	conn   sqlx.SqlConn
	sender *fakeSender
	relay  *Relay
}

func newBusFixture(t *testing.T, maxRetry int) *busFixture {
	t.Helper()
	dsn, _ := testinfra.MySQL(t)
	conn := sqlx.NewMysql(dsn)
	for _, ddl := range Schema {
		if _, err := conn.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}
	for _, table := range []string{"event_outbox", "event_dedup", "event_retry", "event_dead"} {
		if _, err := conn.Exec(fmt.Sprintf("DELETE FROM %s", table)); err != nil {
			t.Fatalf("清表失败: %v", err)
		}
	}
	sender := &fakeSender{}
	relay := NewRelay(conn, sender, RelayConf{MaxRetry: maxRetry})
	return &busFixture{conn: conn, sender: sender, relay: relay}
}

func (f *busFixture) forceRetryDue(t *testing.T) {
	t.Helper()
	for _, table := range []string{"event_outbox", "event_retry"} {
		if _, err := f.conn.Exec(fmt.Sprintf(
			"UPDATE %s SET next_retry_at = DATE_SUB(NOW(3), INTERVAL 1 SECOND) WHERE status IN ('FAILED','RETRY')", table)); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *busFixture) outboxStatus(t *testing.T, eventID string) string {
	t.Helper()
	var row struct {
		Status string `db:"status"`
	}
	if err := f.conn.QueryRowCtx(t.Context(), &row,
		"SELECT status FROM event_outbox WHERE event_id = ?", eventID); err != nil {
		t.Fatalf("查 outbox 失败: %v", err)
	}
	return row.Status
}

func (f *busFixture) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var row struct {
		C int `db:"c"`
	}
	if err := f.conn.QueryRowCtx(t.Context(), &row, query, args...); err != nil {
		t.Fatal(err)
	}
	return row.C
}

// 验收场景 1:全链投递——业务事务内 Emit → Relay(SKIP LOCKED)→ Sender。
func TestFullDelivery(t *testing.T) {
	f := newBusFixture(t, 16)
	ctx := context.Background()

	err := f.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		// 模拟业务写库与事件同事务(dedup 表代位)
		if _, err := session.ExecCtx(ctx,
			"INSERT INTO event_dedup (consumer_group, event_id) VALUES ('biz', 'biz-row-1')"); err != nil {
			return err
		}
		return Emit(ctx, session, EmitInput{
			Topic:    TopicOrderCreated,
			Type:     "order.created",
			Key:      "order-1001",
			TenantID: 42,
			Payload:  map[string]any{"order_no": "order-1001", "amount": "199.00"},
		})
	})
	if err != nil {
		t.Fatalf("事务内 Emit 失败: %v", err)
	}

	delivered, requeued, err := f.relay.DispatchOnce(ctx)
	if err != nil || delivered != 1 || requeued != 0 {
		t.Fatalf("投递结果不符: delivered=%d requeued=%d err=%v", delivered, requeued, err)
	}
	sent := f.sender.all()
	if len(sent) != 1 || sent[0].topic != TopicOrderCreated || sent[0].key != "order-1001" {
		t.Fatalf("投递内容不符: %+v", sent)
	}
	env, err := ParseEnvelope(sent[0].value)
	if err != nil {
		t.Fatalf("信封解析失败: %v", err)
	}
	if env.EventType != "order.created" || env.TenantID != 42 || len(env.Payload) == 0 {
		t.Fatalf("信封字段不符: %+v", env)
	}
	if env.EventID == "" || env.OccurredAt.IsZero() {
		t.Fatal("信封缺 event_id/occurred_at")
	}
	if status := f.outboxStatus(t, env.EventID); status != StatusSent {
		t.Fatalf("状态 = %s, want SENT", status)
	}
	// 再扫一轮不重复投递
	delivered, _, err = f.relay.DispatchOnce(ctx)
	if err != nil || delivered != 0 {
		t.Fatalf("重复扫描不应再投递: delivered=%d err=%v", delivered, err)
	}
}

// 验收场景 2:重复消费——event_dedup 同事务去重。
func TestDuplicateConsumption(t *testing.T) {
	f := newBusFixture(t, 16)

	calls := 0
	sub, err := NewSubscriber(f.conn, func(ctx context.Context, env *Envelope) error {
		calls++
		return nil
	}, SubConf{Group: "group-biz"})
	if err != nil {
		t.Fatal(err)
	}

	raw := mustEnvelope(t, "evt-dup-1", "stock.changed", 7)
	if err := sub.Handle("k1", raw); err != nil {
		t.Fatalf("首次消费失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("首次应执行业务: %d", calls)
	}
	if err := sub.Handle("k1", raw); err != nil {
		t.Fatalf("重复消费应静默: %v", err)
	}
	if calls != 1 {
		t.Fatalf("重复消费不应执行业务: %d", calls)
	}
	// 同事务性:handler 失败时 dedup 行一并回滚 → 下次可重投
	failSub, err := NewSubscriber(f.conn, func(ctx context.Context, env *Envelope) error {
		return errors.New("业务处理失败")
	}, SubConf{Group: "group-biz"})
	if err != nil {
		t.Fatal(err)
	}
	raw2 := mustEnvelope(t, "evt-dup-2", "stock.changed", 7)
	_ = failSub.Handle("k1", raw2)
	if n := f.countRows(t, "SELECT COUNT(*) AS c FROM event_dedup WHERE event_id='evt-dup-2'"); n != 0 {
		t.Fatalf("handler 失败时 dedup 行应回滚: %d", n)
	}
}

// 验收场景 3:失败重投——消费失败 → event_retry → Relay 退避重投 → 消费成功清理。
func TestConsumeFailureRedelivery(t *testing.T) {
	f := newBusFixture(t, 16)

	fail := true
	sub, err := NewSubscriber(f.conn, func(ctx context.Context, env *Envelope) error {
		if fail {
			return errors.New("下游服务暂时不可用")
		}
		return nil
	}, SubConf{Group: "group-retry", MaxRetry: 16})
	if err != nil {
		t.Fatal(err)
	}

	raw := mustEnvelope(t, "evt-retry-1", "payment.settled", 0)
	// 首次消费失败:不返回错误(ack),记入 event_retry
	if err := sub.Handle("k1", raw); err != nil {
		t.Fatalf("失败消费不应向 kq 返回错误: %v", err)
	}
	var row struct {
		RetryCount int    `db:"retry_count"`
		Status     string `db:"status"`
	}
	if err := f.conn.QueryRowCtx(t.Context(), &row,
		"SELECT retry_count, status FROM event_retry WHERE consumer_group='group-retry' AND event_id='evt-retry-1'"); err != nil {
		t.Fatalf("应建重投行: %v", err)
	}
	if row.RetryCount != 1 || row.Status != StatusRetry {
		t.Fatalf("重投行不符: %+v", row)
	}

	// 时间推进到重投时刻 → Relay 重投 → 消费侧成功 → 重投行清理
	f.forceRetryDue(t)
	_, requeued, err := f.relay.DispatchOnce(t.Context())
	if err != nil || requeued != 1 {
		t.Fatalf("应重投 1 条: requeued=%d err=%v", requeued, err)
	}
	if f.sender.count() != 1 {
		t.Fatalf("Sender 应收到重投: %d", f.sender.count())
	}
	fail = false
	if err := sub.Handle("k2", f.sender.all()[0].value); err != nil {
		t.Fatalf("重投消费失败: %v", err)
	}
	if n := f.countRows(t, "SELECT COUNT(*) AS c FROM event_retry WHERE event_id='evt-retry-1'"); n != 0 {
		t.Fatalf("成功消费后重投行应删除: %d", n)
	}
}

// 验收场景 4:dead 表——重投达上限转死信 + 告警钩子。
func TestDeadLetterAfterMaxRetry(t *testing.T) {
	f := newBusFixture(t, 3) // 测试用小上限:3 次重投即死信
	var deadEvents []DeadEvent
	var mu sync.Mutex
	sub, err := NewSubscriber(f.conn, func(ctx context.Context, env *Envelope) error {
		return errors.New("永久失败")
	}, SubConf{Group: "group-dead", MaxRetry: 3},
		WithConsumeDeadHook(func(ev DeadEvent) {
			mu.Lock()
			deadEvents = append(deadEvents, ev)
			mu.Unlock()
		}))
	if err != nil {
		t.Fatal(err)
	}

	raw := mustEnvelope(t, "evt-dead-1", "contract.filed", 3)
	_ = sub.Handle("k1", raw) // 失败 1 → retry_count=1
	f.forceRetryDue(t)
	_, _, _ = f.relay.DispatchOnce(t.Context())
	_ = sub.Handle("k2", f.sender.all()[0].value) // 失败 2 → retry_count=2
	f.forceRetryDue(t)
	_, _, _ = f.relay.DispatchOnce(t.Context())
	_ = sub.Handle("k3", f.sender.all()[1].value) // 失败 3 ≥ max(3) → dead

	if n := f.countRows(t, "SELECT COUNT(*) AS c FROM event_dead WHERE event_id='evt-dead-1' AND source='consume'"); n != 1 {
		t.Fatalf("dead 表应有 1 条: %d", n)
	}
	if n := f.countRows(t, "SELECT COUNT(*) AS c FROM event_retry WHERE event_id='evt-dead-1'"); n != 0 {
		t.Fatalf("转死后重投行应删除: %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deadEvents) != 1 || deadEvents[0].Source != "consume" {
		t.Fatalf("告警钩子未触发: %+v", deadEvents)
	}
}

// outbox 投递失败:退避重投 → 达上限 → DEAD + dead 表(source=outbox)。
func TestOutboxDeliveryFailureToDead(t *testing.T) {
	f := newBusFixture(t, 2)
	ctx := context.Background()
	f.sender.failNext = 99 // Send 持续失败

	if err := Emit(ctx, f.conn, EmitInput{Topic: TopicOrderPaid, Type: "order.paid", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	var row struct {
		EventID string `db:"event_id"`
	}
	if err := f.conn.QueryRowCtx(t.Context(), &row, "SELECT event_id FROM event_outbox LIMIT 1"); err != nil {
		t.Fatal(err)
	}

	// 第 1 轮:失败 → FAILED(+1m)
	if _, _, err := f.relay.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if status := f.outboxStatus(t, row.EventID); status != StatusFailed {
		t.Fatalf("状态 = %s, want FAILED", status)
	}
	// 第 2 轮:失败 → 达上限(2)→ DEAD
	f.forceRetryDue(t)
	if _, _, err := f.relay.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if status := f.outboxStatus(t, row.EventID); status != StatusDead {
		t.Fatalf("状态 = %s, want DEAD", status)
	}
	if n := f.countRows(t, "SELECT COUNT(*) AS c FROM event_dead WHERE event_id=? AND source='outbox'", row.EventID); n != 1 {
		t.Fatalf("dead 表应有 outbox 死信: %d", n)
	}
}

// 畸形消息:记轨迹后 ack,不无限重试。
func TestMalformedMessage(t *testing.T) {
	f := newBusFixture(t, 16)
	sub, err := NewSubscriber(f.conn, func(ctx context.Context, env *Envelope) error {
		return nil
	}, SubConf{Group: "group-malformed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Handle("k1", "{not-json"); err != nil {
		t.Fatalf("畸形消息应 ack 不报错: %v", err)
	}
	if n := f.countRows(t, "SELECT COUNT(*) AS c FROM event_dead WHERE source='malformed'"); n != 1 {
		t.Fatalf("畸形消息应入 dead 表: %d", n)
	}
}

// 消费侧上下文恢复:信封 tenant_id → ctx(租户经 tenantx 恢复)。
func TestSubscriberContextRestore(t *testing.T) {
	f := newBusFixture(t, 16)
	var gotTenant int64
	var tenantOK bool
	sub, err := NewSubscriber(f.conn, func(ctx context.Context, env *Envelope) error {
		gotTenant, tenantOK = tenantx.TenantFromCtx(ctx)
		return nil
	}, SubConf{Group: "group-ctx"})
	if err != nil {
		t.Fatal(err)
	}
	raw := mustEnvelope(t, "evt-ctx-1", "party.updated", 88)
	if err := sub.Handle("k1", raw); err != nil {
		t.Fatal(err)
	}
	if !tenantOK || gotTenant != 88 {
		t.Fatalf("租户恢复失败: %d %v", gotTenant, tenantOK)
	}
}

// Emit 校验:topic/type 必填。
func TestEmitValidation(t *testing.T) {
	f := newBusFixture(t, 16)
	ctx := context.Background()
	if err := Emit(ctx, f.conn, EmitInput{Type: "x"}); err == nil {
		t.Fatal("缺 topic 应报错")
	}
	if err := Emit(ctx, f.conn, EmitInput{Topic: TopicAuditEvent}); err == nil {
		t.Fatal("缺 type 应报错")
	}
}

// —— 辅助 ——

func mustEnvelope(t *testing.T, eventID, eventType string, tenantID int64) string {
	t.Helper()
	raw, err := json.Marshal(&Envelope{
		EventID:    eventID,
		EventType:  eventType,
		TenantID:   tenantID,
		OccurredAt: time.Now().UTC(),
		Payload:    []byte(`{"x":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
