package eventbus

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-queue/kq"
)

// kq Pusher 封装(02 §6.6 / ADR-12):
//   - PushWithKey 保序:同聚合(order_no/sn)同 key 同分区,per-aggregate 有序;
//   - 同步发送:Push 每条直接写 Kafka(未开启 chunk 批量),MQ 成功才返回(E3 口径
//     "MQ ack 后才 ack MQTT" 同款纪律);
//   - Close Flush:进程退出前 Close 刷缓冲,缓冲不丢。
//
// Relay 的 Sender 标准实现是 KqSender(多 topic 分发)。

// KqPusher 单 topic 推送器。
type KqPusher struct {
	p     *kq.Pusher
	topic string
}

// NewKqPusher 构造(brokers 如 127.0.0.1:29092)。
func NewKqPusher(brokers []string, topic string) (*KqPusher, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("eventbus: kafka brokers 为空")
	}
	if topic == "" {
		return nil, fmt.Errorf("eventbus: topic 为空")
	}
	return &KqPusher{p: kq.NewPusher(brokers, topic), topic: topic}, nil
}

// Topic 目标 topic。
func (k *KqPusher) Topic() string { return k.topic }

// PushWithKey 按 key 保序推送(同步)。
func (k *KqPusher) PushWithKey(ctx context.Context, key, value string) error {
	return k.p.PushWithKey(ctx, key, value)
}

// Send 实现 Sender(单 topic 绑定,topic 不符即拒绝——防发错队列)。
func (k *KqPusher) Send(ctx context.Context, topic, key, value string) error {
	if topic != k.topic {
		return fmt.Errorf("eventbus: pusher 绑定 %s,拒绝投递 %s", k.topic, topic)
	}
	return k.p.PushWithKey(ctx, key, value)
}

// Close Flush 关闭(优雅退出调用)。
func (k *KqPusher) Close() error { return k.p.Close() }

// KqSender 多 topic 分发器(Sender 的 kq 标准实现)。
type KqSender struct {
	pushers map[string]*KqPusher
}

// NewKqSender 为每个 topic 建一个 pusher。
func NewKqSender(brokers []string, topics []string) (*KqSender, error) {
	if len(topics) == 0 {
		return nil, fmt.Errorf("eventbus: topics 为空")
	}
	pushers := make(map[string]*KqPusher, len(topics))
	for _, topic := range topics {
		p, err := NewKqPusher(brokers, topic)
		if err != nil {
			return nil, err
		}
		pushers[topic] = p
	}
	return &KqSender{pushers: pushers}, nil
}

// Send 按 topic 分发。
func (s *KqSender) Send(ctx context.Context, topic, key, value string) error {
	p, ok := s.pushers[topic]
	if !ok {
		return fmt.Errorf("eventbus: topic %s 未注册 Sender(检查 NewKqSender topics)", topic)
	}
	return p.PushWithKey(ctx, key, value)
}

// Close 依次 Flush 关闭。
func (s *KqSender) Close() error {
	var firstErr error
	for _, p := range s.pushers {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
