package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Sender 事件投递口(Relay 的传输层抽象;kq.KqSender 实现,测试用内存实现)。
type Sender interface {
	// Send 投递一条事件到指定 topic,key 用于分区保序(同聚合同 key 同分区)。
	Send(ctx context.Context, topic, key, value string) error
}

// DeadEvent 死信条目(onDead 告警钩子的入参)。
type DeadEvent struct {
	Source     string
	EventID    string
	Group      string
	EventType  string
	Topic      string
	TenantID   int64
	RetryCount int
	LastError  string
}

// RelayConf Relay 配置。
type RelayConf struct {
	// Interval 扫描间隔(默认 1s;cron 注册表登记后由调度器驱动亦可)。
	Interval time.Duration `json:",default=1s"`
	// BatchSize 单次批量投递上限(默认 100)。
	BatchSize int `json:",default=100"`
	// MaxRetry 重投上限(默认 16,02 §6.6:1m/5m/30m ≤16 次 → dead 表)。
	MaxRetry int `json:",default=16"`
}

func (c RelayConf) withDefaults() RelayConf {
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.MaxRetry <= 0 {
		c.MaxRetry = 16
	}
	return c
}

// Relay outbox/重投表的轮询投递器。
// 多实例安全:候选行以 FOR UPDATE SKIP LOCKED 认领,互不重复投递。
type Relay struct {
	conn   sqlx.SqlConn
	sender Sender
	conf   RelayConf
	onDead func(DeadEvent)
}

// Option Relay 选项。
type Option func(*Relay)

// WithDeadHook 死信告警钩子(服务接 Prometheus eventbus_dead_events_total + 通知)。
func WithDeadHook(fn func(DeadEvent)) Option {
	return func(r *Relay) { r.onDead = fn }
}

// NewRelay 构造。
func NewRelay(conn sqlx.SqlConn, sender Sender, conf RelayConf, opts ...Option) *Relay {
	r := &Relay{conn: conn, sender: sender, conf: conf.withDefaults()}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run 常驻轮询(ctx 取消退出;服务侧经 service.ServiceGroup 注册,E4)。
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.conf.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, _, err := r.DispatchOnce(ctx); err != nil {
				logx.Errorf("eventbus: relay 轮询失败: %v", err)
			}
		}
	}
}

// DispatchOnce 单轮扫描(outbox 投递 + 消费失败重投),返回 (投递数, 重投数, 错误)。
// 独立导出:测试直接驱动,服务可挂进 cron 注册表(登记 + last_run 指标,E16)。
func (r *Relay) DispatchOnce(ctx context.Context) (delivered int, requeued int, err error) {
	delivered, err = r.dispatchOutbox(ctx)
	if err != nil {
		return delivered, 0, err
	}
	requeued, err = r.dispatchRetries(ctx)
	return delivered, requeued, err
}

// dispatchOutbox 拉起 PENDING/FAILED 且到期的行 → Send → SENT/FAILED/DEAD。
func (r *Relay) dispatchOutbox(ctx context.Context) (int, error) {
	delivered := 0
	err := r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		var rows []outboxRow
		if err := session.QueryRowsCtx(ctx, &rows,
			`SELECT id, event_id, event_type, topic, partition_key, tenant_id, trace_id, payload, retry_count
			 FROM event_outbox
			 WHERE status IN (?, ?) AND next_retry_at <= NOW(3)
			 ORDER BY next_retry_at
			 LIMIT ? FOR UPDATE SKIP LOCKED`,
			StatusPending, StatusFailed, r.conf.BatchSize); err != nil {
			return err
		}
		for _, row := range rows {
			raw := row.marshalEnvelope()
			sendErr := r.sender.Send(ctx, row.Topic, row.PartitionKey, raw)
			if sendErr == nil {
				if _, err := session.ExecCtx(ctx,
					`UPDATE event_outbox SET status = ?, last_error = '' WHERE id = ?`,
					StatusSent, row.ID); err != nil {
					return err
				}
				delivered++
				continue
			}
			if err := r.markOutboxFailure(ctx, session, row, sendErr); err != nil {
				return err
			}
		}
		return nil
	})
	return delivered, err
}

type outboxRow struct {
	ID           int64  `db:"id"`
	EventID      string `db:"event_id"`
	EventType    string `db:"event_type"`
	Topic        string `db:"topic"`
	PartitionKey string `db:"partition_key"`
	TenantID     int64  `db:"tenant_id"`
	TraceID      string `db:"trace_id"`
	Payload      string `db:"payload"`
	RetryCount   int    `db:"retry_count"`
}

// marshalEnvelope 从 outbox 行还原信封 JSON(payload 列即信封全文)。
func (row outboxRow) marshalEnvelope() string {
	// payload 已是完整信封 JSON(Emit 时写入);分片键在行上,不在信封里
	return row.Payload
}

// markOutboxFailure 退避记账:1m/5m/30m 循环退避,达上限转 DEAD + dead 表 + 告警。
func (r *Relay) markOutboxFailure(ctx context.Context, session sqlx.Session, row outboxRow, sendErr error) error {
	newCount := row.RetryCount + 1
	errText := truncateError(sendErr)
	if newCount >= r.conf.MaxRetry {
		if _, err := session.ExecCtx(ctx,
			`UPDATE event_outbox SET status = ?, retry_count = ?, last_error = ? WHERE id = ?`,
			StatusDead, newCount, errText, row.ID); err != nil {
			return err
		}
		if _, err := session.ExecCtx(ctx,
			`INSERT INTO event_dead (source, event_id, consumer_group, event_type, topic, partition_key, tenant_id, trace_id, payload, retry_count, last_error)
			 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`,
			"outbox", row.EventID, row.EventType, row.Topic, row.PartitionKey, row.TenantID, row.TraceID, row.Payload, newCount, errText); err != nil {
			return err
		}
		r.fireDead(DeadEvent{
			Source: "outbox", EventID: row.EventID, EventType: row.EventType,
			Topic: row.Topic, TenantID: row.TenantID, RetryCount: newCount, LastError: errText,
		})
		return nil
	}
	if _, err := session.ExecCtx(ctx,
		`UPDATE event_outbox SET status = ?, retry_count = ?, next_retry_at = DATE_ADD(NOW(3), INTERVAL ? SECOND), last_error = ?
		 WHERE id = ?`,
		StatusFailed, newCount, backoffSeconds(newCount), errText, row.ID); err != nil {
		return err
	}
	return nil
}

// dispatchRetries 拉起到期重投行 → Send(成功后行保留,由消费成功侧删除;
// 消费再失败由 Subscriber 计数退避,达上限入 dead 表)。
func (r *Relay) dispatchRetries(ctx context.Context) (int, error) {
	requeued := 0
	err := r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		var rows []retryRow
		if err := session.QueryRowsCtx(ctx, &rows,
			`SELECT id, event_id, consumer_group, event_type, topic, partition_key, tenant_id, payload, retry_count
			 FROM event_retry
			 WHERE status = ? AND next_retry_at <= NOW(3)
			 ORDER BY next_retry_at
			 LIMIT ? FOR UPDATE SKIP LOCKED`,
			StatusRetry, r.conf.BatchSize); err != nil {
			return err
		}
		for _, row := range rows {
			if err := r.sender.Send(ctx, row.Topic, row.PartitionKey, row.Payload); err != nil {
				// Send 失败:推迟本轮重投(Send 层故障,不累计消费失败次数)
				if _, err := session.ExecCtx(ctx,
					`UPDATE event_retry SET next_retry_at = DATE_ADD(NOW(3), INTERVAL ? SECOND) WHERE id = ?`,
					backoffSeconds(row.RetryCount+1), row.ID); err != nil {
					return err
				}
				continue
			}
			// 重投成功:按次数排下一次(若消息已被成功消费,行将被 Subscriber 删除,本更新为空操作)
			if _, err := session.ExecCtx(ctx,
				`UPDATE event_retry SET next_retry_at = DATE_ADD(NOW(3), INTERVAL ? SECOND) WHERE id = ?`,
				backoffSeconds(row.RetryCount+1), row.ID); err != nil {
				return err
			}
			requeued++
		}
		return nil
	})
	return requeued, err
}

type retryRow struct {
	ID            int64  `db:"id"`
	EventID       string `db:"event_id"`
	ConsumerGroup string `db:"consumer_group"`
	EventType     string `db:"event_type"`
	Topic         string `db:"topic"`
	PartitionKey  string `db:"partition_key"`
	TenantID      int64  `db:"tenant_id"`
	Payload       string `db:"payload"`
	RetryCount    int    `db:"retry_count"`
}

// backoffSeconds 退避计划:第 1 次重投 +1m,第 2 次 +5m,其后 +30m(02 §6.6)。
func backoffSeconds(retryCount int) int {
	switch retryCount {
	case 1:
		return 60
	case 2:
		return 300
	default:
		return 1800
	}
}

func (r *Relay) fireDead(ev DeadEvent) {
	logx.Errorf("eventbus: 事件进入死信 source=%s event_id=%s topic=%s retries=%d err=%s",
		ev.Source, ev.EventID, ev.Topic, ev.RetryCount, ev.LastError)
	if r.onDead != nil {
		r.onDead(ev)
	}
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// ErrAlreadyConsumed 重复投递的幂等跳过信号。
var ErrAlreadyConsumed = errors.New("eventbus: 事件已被消费(去重命中)")

// ParseEnvelope 从原始消息解析信封(消费侧/排障用)。
func ParseEnvelope(raw string) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("eventbus: 解析信封失败: %w", err)
	}
	if env.EventID == "" {
		return nil, errors.New("eventbus: 信封缺少 event_id")
	}
	return &env, nil
}
