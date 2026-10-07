// Package snowflake 实现分布式 ID:41bit 时间戳(2026 纪元)+ 10bit worker + 12bit 序列。
//
// worker_id 分配(02 §8):etcd 租约抢占(进程退出释放,实例崩溃由租约 TTL 兜底回收),
// etcd 不可用退化环境变量 MICRO_WORKER_ID——冲突不允许静默吞掉。
package snowflake

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

const (
	// Epoch 起始纪元:2026-01-01 00:00:00 UTC(毫秒)。
	Epoch int64 = 1767225600000

	workerBits = 10
	seqBits    = 12

	// MaxWorker worker_id 上限(10bit,0~1023)。
	MaxWorker = -1 ^ (-1 << workerBits)
	// MaxSeq 单毫秒序列上限(12bit,单机 409 万 QPS)。
	MaxSeq = -1 ^ (-1 << seqBits)
)

var (
	// ErrWorkerInvalid worker_id 越界。
	ErrWorkerInvalid = fmt.Errorf("snowflake: worker_id 越界(0~%d)", MaxWorker)
	// ErrClockBackward 时钟回拨:拒绝发号,冲突不允许静默。
	ErrClockBackward = errors.New("snowflake: 检测到时钟回拨,拒绝发号")
)

// Node 雪花发号器实例。
type Node struct {
	mu       sync.Mutex
	workerID int64
	lastTS   int64
	seq      int64
}

// New 以指定 worker_id 构造发号器(部署平台静态指定场景)。
func New(workerID int64) (*Node, error) {
	if workerID < 0 || workerID > MaxWorker {
		return nil, ErrWorkerInvalid
	}
	return &Node{workerID: workerID}, nil
}

// NextID 生成趋势递增的 int64 ID。
func (n *Node) NextID() (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now().UnixMilli()
	if now < n.lastTS {
		return 0, fmt.Errorf("%w: 回拨 %dms", ErrClockBackward, n.lastTS-now)
	}
	if now == n.lastTS {
		n.seq = (n.seq + 1) & MaxSeq
		if n.seq == 0 { // 当前毫秒序列耗尽,自旋等待下一毫秒
			for now <= n.lastTS {
				now = time.Now().UnixMilli()
			}
		}
	} else {
		n.seq = 0
	}
	n.lastTS = now

	return (now-Epoch)<<(workerBits+seqBits) | n.workerID<<seqBits | n.seq, nil
}

// MustNextID NextID 的忽略错误变体:仅时钟回拨可能报错,业务侧通常直接拒绝请求。
func (n *Node) MustNextID() int64 {
	id, err := n.NextID()
	if err != nil {
		panic(err)
	}
	return id
}

// WorkerID 返回本节点 worker_id。
func (n *Node) WorkerID() int64 { return n.workerID }

// ParseID 解析 ID 成分(排障:看 ID 知生成时间与机器)。
func ParseID(id int64) (ts time.Time, workerID int64, seq int64) {
	ts = time.UnixMilli(Epoch + (id >> (workerBits + seqBits)))
	workerID = (id >> seqBits) & MaxWorker
	seq = id & MaxSeq
	return
}

// ParseWorkerID 解析 MICRO_WORKER_ID 环境变量值。
func ParseWorkerID(raw string) (int64, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("snowflake: MICRO_WORKER_ID %q 不是合法整数: %w", raw, err)
	}
	if v < 0 || v > MaxWorker {
		return 0, ErrWorkerInvalid
	}
	return v, nil
}
