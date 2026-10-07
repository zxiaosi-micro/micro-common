package eventbus

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"go.opentelemetry.io/otel/trace"

	"github.com/zxiaosi-micro/micro-common/tenantx"
)

// Subscriber 消费侧封装:
//
//	Handle(key, val) 即 kq.Handler 语义(Consume),永不主动返回错误——
//	消费失败 → event_retry 表(Relay 按 next_retry_at 退避重投 ≤16 次 → dead 表);
//	畸形消息 → 记轨迹进 dead 表后 ack(不无限重试);
//	重复投递 → event_dedup 唯一约束幂等跳过。
//
// 依赖 DB 故障时 Handle 返回 error,由 kq 层 ack 语义兜底(offset 不推进)。
type Subscriber struct {
	conn     sqlx.SqlConn
	group    string
	handler  Handler
	maxRetry int
	onDead   func(DeadEvent)
}

// Handler 业务消费函数(去重事务内执行;失败=同事务回滚,dedup 行也不落库)。
type Handler func(ctx context.Context, env *Envelope) error

// SubConf Subscriber 配置。
type SubConf struct {
	// Group 消费组名(全局唯一,E2)。
	Group string
	// MaxRetry 消费失败重投上限(默认 16)。
	MaxRetry int
}

// NewSubscriber 构造。
func NewSubscriber(conn sqlx.SqlConn, handler Handler, conf SubConf, opts ...func(*Subscriber)) (*Subscriber, error) {
	if conf.Group == "" {
		return nil, errors.New("eventbus: 消费组 Group 不允许为空(全局唯一,E2)")
	}
	if handler == nil {
		return nil, errors.New("eventbus: handler 不允许为空")
	}
	if conf.MaxRetry <= 0 {
		conf.MaxRetry = 16
	}
	s := &Subscriber{conn: conn, group: conf.Group, handler: handler, maxRetry: conf.MaxRetry}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// WithConsumeDeadHook 消费侧死信告警钩子。
func WithConsumeDeadHook(fn func(DeadEvent)) func(*Subscriber) {
	return func(s *Subscriber) { s.onDead = fn }
}

// Consume 使 *Subscriber 直接满足 kq.Handler 接口(kq.MustNewQueue 可传 s)。
func (s *Subscriber) Consume(key, val string) error { return s.Handle(key, val) }

// Handle kq 消费入口(kq.MustNewQueue 的 handler 传 s)。
func (s *Subscriber) Handle(_ /*key 已在信封 partition_key 中,业务不用*/ string, val string) error {
	env, err := ParseEnvelope(val)
	if err != nil {
		// 畸形消息:记轨迹后 ack,不无限重试(02 §6.6)
		s.recordMalformed(val, err)
		return nil
	}
	ctx := s.restoreContext(env)

	// dedup 插入 + 业务处理同事务:失败一起回滚,dedup 行不落库 → 可安全重投
	txErr := s.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		if err := markConsumed(ctx, session, s.group, env); err != nil {
			return err
		}
		return s.handler(ctx, env)
	})
	switch {
	case txErr == nil:
		s.clearRetry(env) // 消费成功:清理重投记录(若有)
		return nil
	case errors.Is(txErr, ErrAlreadyConsumed):
		return nil // 重复投递:幂等跳过
	default:
		if rerr := s.recordConsumeFailure(env, txErr); rerr != nil {
			// 重投记录都写不进(DB 故障):返回错误,kq 层不 ack
			return fmt.Errorf("eventbus: 记录消费失败失败(原始错误: %v): %w", txErr, rerr)
		}
		return nil // ack;重投交给 Relay(退避 1m/5m/30m)
	}
}

// markConsumed 去重落库:唯一约束冲突 → ErrAlreadyConsumed。
func markConsumed(ctx context.Context, session sqlx.Session, group string, env *Envelope) error {
	_, err := session.ExecCtx(ctx,
		`INSERT INTO event_dedup (consumer_group, event_id) VALUES (?, ?)`, group, env.EventID)
	if err != nil {
		if isDuplicateEntry(err) {
			return ErrAlreadyConsumed
		}
		return fmt.Errorf("eventbus: 写去重记录失败: %w", err)
	}
	return nil
}

func isDuplicateEntry(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1062
	}
	return false
}

// restoreContext 恢复链路与租户:trace_id 还原为远端 span 上下文
// (go-zero otel 自动把 trace_id 注入后续日志),tenant_id 进 ctxkit/tenantx。
func (s *Subscriber) restoreContext(env *Envelope) context.Context {
	ctx := context.Background()
	if tid, err := trace.TraceIDFromHex(env.TraceID); err == nil && tid.IsValid() {
		var sid trace.SpanID
		_, _ = rand.Read(sid[:])
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    tid,
			SpanID:     sid,
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		})
		ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
	}
	if env.TenantID > 0 {
		ctx = tenantx.WithTenant(ctx, env.TenantID)
	}
	return ctx
}

// clearRetry 消费成功后清理重投行。
func (s *Subscriber) clearRetry(env *Envelope) {
	_, err := s.conn.ExecCtx(context.WithoutCancel(context.Background()),
		`DELETE FROM event_retry WHERE consumer_group = ? AND event_id = ?`, s.group, env.EventID)
	if err != nil {
		logx.Errorf("eventbus: 清理重投行失败 event_id=%s: %v", env.EventID, err)
	}
}

// recordConsumeFailure 记消费失败:首败建行,再败计数退避,达上限转 dead 表。
func (s *Subscriber) recordConsumeFailure(env *Envelope, cause error) error {
	ctx := context.WithoutCancel(context.Background()) // 后台记账,不挂请求 ctx(E5)
	errText := truncateError(cause)
	var row struct {
		RetryCount int `db:"retry_count"`
	}
	qErr := s.conn.QueryRowCtx(ctx, &row,
		`SELECT retry_count FROM event_retry WHERE consumer_group = ? AND event_id = ? FOR UPDATE`,
		s.group, env.EventID)
	var exists bool
	switch {
	case qErr == nil:
		exists = true
	case errors.Is(qErr, sql.ErrNoRows):
		exists = false
	default:
		return fmt.Errorf("eventbus: 查重投行失败: %w", qErr)
	}

	if !exists {
		raw := marshalEnvelopeRaw(env)
		_, err := s.conn.ExecCtx(ctx,
			`INSERT INTO event_retry (event_id, consumer_group, event_type, topic, partition_key, tenant_id, trace_id, payload, status, retry_count, next_retry_at, last_error)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, DATE_ADD(NOW(3), INTERVAL ? SECOND), ?)`,
			env.EventID, s.group, env.EventType, "", "", env.TenantID, env.TraceID, raw, StatusRetry,
			backoffSeconds(1), errText)
		if err != nil {
			return fmt.Errorf("eventbus: 建重投行失败: %w", err)
		}
		return nil
	}

	newCount := row.RetryCount + 1
	if newCount >= s.maxRetry {
		if err := s.moveToDead(ctx, env, newCount, errText); err != nil {
			return err
		}
		return nil
	}
	if _, err := s.conn.ExecCtx(ctx,
		`UPDATE event_retry SET retry_count = ?, status = ?, next_retry_at = DATE_ADD(NOW(3), INTERVAL ? SECOND), last_error = ?
		 WHERE consumer_group = ? AND event_id = ?`,
		newCount, StatusRetry, backoffSeconds(newCount), errText, s.group, env.EventID); err != nil {
		return fmt.Errorf("eventbus: 更新重投行失败: %w", err)
	}
	return nil
}

// moveToDead 重投达上限:入 dead 表 + 删重投行 + 告警。
func (s *Subscriber) moveToDead(ctx context.Context, env *Envelope, count int, errText string) error {
	raw := marshalEnvelopeRaw(env)
	err := s.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, err := session.ExecCtx(ctx,
			`INSERT INTO event_dead (source, event_id, consumer_group, event_type, topic, tenant_id, trace_id, payload, retry_count, last_error)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"consume", env.EventID, s.group, env.EventType, "", env.TenantID, env.TraceID, raw, count, errText); err != nil {
			return err
		}
		_, err := session.ExecCtx(ctx,
			`DELETE FROM event_retry WHERE consumer_group = ? AND event_id = ?`, s.group, env.EventID)
		return err
	})
	if err != nil {
		return fmt.Errorf("eventbus: 转死信失败: %w", err)
	}
	ev := DeadEvent{
		Source: "consume", EventID: env.EventID, Group: s.group, EventType: env.EventType,
		TenantID: env.TenantID, RetryCount: count, LastError: errText,
	}
	logx.Errorf("eventbus: 事件进入死信 source=consume event_id=%s group=%s retries=%d err=%s",
		ev.EventID, ev.Group, ev.RetryCount, ev.LastError)
	if s.onDead != nil {
		s.onDead(ev)
	}
	return nil
}

// recordMalformed 畸形消息:记轨迹进 dead 表后 ack。
func (s *Subscriber) recordMalformed(raw string, cause error) {
	ctx := context.WithoutCancel(context.Background())
	// payload 列为 JSON 类型:畸形原文必须包装成 JSON 字符串才能入库留痕
	rawJSON, merr := json.Marshal(raw)
	if merr != nil {
		rawJSON = []byte(`""`)
	}
	_, err := s.conn.ExecCtx(ctx,
		`INSERT INTO event_dead (source, event_id, consumer_group, payload, last_error)
		 VALUES (?, ?, ?, ?, ?)`,
		"malformed", NewEventID(), s.group, string(rawJSON), truncateError(cause))
	if err != nil {
		logx.Errorf("eventbus: 畸形消息留痕失败(消息已丢弃): %v, raw=%.256s", err, raw)
		return
	}
	logx.Errorf("eventbus: 畸形消息已记轨迹后丢弃 group=%s err=%v", s.group, cause)
	if s.onDead != nil {
		s.onDead(DeadEvent{Source: "malformed", Group: s.group, LastError: truncateError(cause)})
	}
}

// marshalEnvelopeRaw 信封 → JSON(重投行存信封全文,重投即重发)。
func marshalEnvelopeRaw(env *Envelope) string {
	raw, err := json.Marshal(env)
	if err != nil {
		logx.Errorf("eventbus: 重投信封序列化失败: %v", err)
		return "{}"
	}
	return string(raw)
}
