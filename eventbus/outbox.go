package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Outbox 状态机:
//
//	PENDING(业务事务内写入)→ SENT(Relay 投递成功)
//	                       → FAILED(投递失败,退避重投)→ SENT / DEAD(≤16 次)
//	DEAD → dead 表留底 + 告警,人工经补投递重放处置(禁止裸删,E10)

// outboxStatus 事件状态。
const (
	StatusPending = "PENDING"
	StatusSent    = "SENT"
	StatusFailed  = "FAILED"
	StatusDead    = "DEAD"
	StatusRetry   = "RETRY" // event_retry 行(消费失败重投)
)

// Emit 在业务事务内写入事件(outbox 表)。
// db 传 sqlx.TransactCtx 回调的 session(强约束:与业务变更同事务提交);
// 传 SqlConn(自动提交)仅限无本地事务的独立事件,评审需说明理由。
func Emit(ctx context.Context, db sqlx.Session, in EmitInput) error {
	if in.Topic == "" || in.Type == "" {
		return fmt.Errorf("eventbus: Emit 缺少 topic/event_type")
	}
	if in.EventID == "" {
		in.EventID = NewEventID()
	}
	payload, err := json.Marshal(in.Payload)
	if err != nil {
		return fmt.Errorf("eventbus: 序列化 payload 失败: %w", err)
	}
	env := Envelope{
		EventID:    in.EventID,
		EventType:  in.Type,
		TenantID:   in.TenantID,
		TraceID:    TraceIDFromCtx(ctx),
		OccurredAt: time.Now(),
		Payload:    payload,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("eventbus: 序列化信封失败: %w", err)
	}
	_, err = db.ExecCtx(ctx,
		`INSERT INTO event_outbox
			(event_id, event_type, topic, partition_key, tenant_id, trace_id, payload, status, retry_count, next_retry_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NOW(3))`,
		env.EventID, env.EventType, in.Topic, in.Key, env.TenantID, env.TraceID, string(raw), StatusPending)
	if err != nil {
		return fmt.Errorf("eventbus: 写 outbox 失败: %w", err)
	}
	return nil
}
