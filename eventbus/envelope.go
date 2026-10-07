// Package eventbus 是平台一致性基石(02 §9.3,ADR-12):
//
//	Outbox(业务事务内 Emit 写事件,杜绝"业务成功但事件丢失")
//	→ Relay(SKIP LOCKED 拉取 → kq 同步投递;投递/消费失败按 next_retry_at
//	   DB 退避重投 1m/5m/30m ≤16 次 → dead 表 + 告警)
//	→ Kafka kq(at-least-once,PushWithKey 保序)
//	→ Subscriber(event_dedup 同事务去重 + trace/租户恢复;at-least-once,
//	   消费侧幂等是前提,不存在 exactly-once)。
//
// 语义声明:这不是 exactly-once——投递侧 at-least-once + 消费侧幂等才是完整正确性。
package eventbus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/zeromicro/go-zero/core/trace"
)

// Envelope 事件信封(跨服务契约,消费侧凭它恢复租户与链路)。
type Envelope struct {
	// EventID 事件唯一标识(去重/重投/对账键)。
	EventID string `json:"event_id"`
	// EventType 事件类型(点号命名,如 order.created;topic 为下划线)。
	EventType string `json:"event_type"`
	// TenantID 租户 ID(消费侧经 tenantx 恢复;0 = 平台级事件)。
	TenantID int64 `json:"tenant_id"`
	// TraceID 生产侧链路 ID(消费侧恢复,日志与 Jaeger 可互跳)。
	TraceID string `json:"trace_id"`
	// OccurredAt 业务发生时间。
	OccurredAt time.Time `json:"occurred_at"`
	// Payload 业务负载(JSON)。
	Payload json.RawMessage `json:"payload"`
}

// NewEventID 128bit 随机 hex 事件 ID。
func NewEventID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// TraceIDFromCtx 取当前链路 ID(无链路时为空串)。
func TraceIDFromCtx(ctx context.Context) string {
	return trace.TraceIDFromContext(ctx)
}

// EmitInput Emit 的入参。
type EmitInput struct {
	// Topic 目标 topic(下划线命名,见 topics.go;须经 tools/mqinit 预创建,E2)。
	Topic string
	// Type 事件类型(点号命名)。
	Type string
	// Key 分区保序键 = 聚合 ID(order_no/sn/saga_id),同聚合同分区有序。
	Key string
	// TenantID 租户 ID。
	TenantID int64
	// Payload 业务负载(任意可 JSON 序列化结构)。
	Payload any
	// EventID 可选;为空自动生成。
	EventID string
}
