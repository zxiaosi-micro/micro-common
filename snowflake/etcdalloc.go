package snowflake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// EnvWorkerID worker_id 退化来源环境变量(附录 C)。
const EnvWorkerID = "MICRO_WORKER_ID"

// DefaultPrefix etcd 抢占 key 前缀(与服务发现/配置中心 prefix 隔离)。
const DefaultPrefix = "/micro/snowflake/worker/"

// DefaultLeaseTTL 租约 TTL:实例崩溃后最多占用该时长即被回收。
const DefaultLeaseTTL = 30 * time.Second

var (
	// ErrWorkersExhausted 全部 worker 槽位被占(1023 实例上限)。
	ErrWorkersExhausted = errors.New("snowflake: worker_id 槽位已耗尽")
	// ErrNoWorkerSource etcd 与环境变量都不可用——拒绝静默随机。
	ErrNoWorkerSource = errors.New("snowflake: worker_id 无可用来源(etcd 不可用且未设置 MICRO_WORKER_ID)")
)

// EtcdAllocator 基于 etcd 租约的 worker_id 抢占分配器。
type EtcdAllocator struct {
	client   *clientv3.Client
	prefix   string
	ttl      int64
	instance string // host:pid,etcd 值,排障可见

	mu      sync.Mutex
	leaseID clientv3.LeaseID
	kaDone  chan struct{}
}

// NewEtcdAllocator 构造分配器(endpoints 为 etcd 地址)。
func NewEtcdAllocator(endpoints []string, prefix string, ttl time.Duration) (*EtcdAllocator, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("snowflake: etcd endpoints 为空")
	}
	if prefix == "" {
		prefix = DefaultPrefix
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("snowflake: 创建 etcd client 失败: %w", err)
	}
	host, _ := os.Hostname()
	return &EtcdAllocator{
		client:   client,
		prefix:   prefix,
		ttl:      int64(ttl / time.Second),
		instance: fmt.Sprintf("%s:%d", host, os.Getpid()),
	}, nil
}

// Acquire 租约 + 逐个槽位事务抢占;成功后 KeepAlive 由后台协程维持。
// etcd 不可达时返回 error(调用方退化环境变量,见 NewAuto)。
func (a *EtcdAllocator) Acquire(ctx context.Context) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.client.Sync(ctx); err != nil {
		return 0, fmt.Errorf("snowflake: etcd 不可达: %w", err)
	}
	lease := clientv3.NewLease(a.client)
	granted, err := lease.Grant(ctx, a.ttl)
	if err != nil {
		return 0, fmt.Errorf("snowflake: 租约授予失败: %w", err)
	}

	for id := int64(0); id <= MaxWorker; id++ {
		key := fmt.Sprintf("%s%d", a.prefix, id)
		txn, err := a.client.Txn(ctx).
			If(clientv3.Compare(clientv3.Version(key), "=", 0)).
			Then(clientv3.OpPut(key, a.instance, clientv3.WithLease(granted.ID))).
			Commit()
		if err != nil {
			return 0, fmt.Errorf("snowflake: 抢占槽位 %d 失败: %w", id, err)
		}
		if txn.Succeeded {
			a.leaseID = granted.ID
			a.keepAlive(ctx, lease, granted.ID, key)
			return id, nil
		}
	}
	_, _ = lease.Revoke(context.WithoutCancel(ctx), granted.ID)
	return 0, ErrWorkersExhausted
}

func (a *EtcdAllocator) keepAlive(ctx context.Context, lease clientv3.Lease, id clientv3.LeaseID, key string) {
	// KeepAlive 生命周期独立于调用方请求 ctx:租约须随进程存活,而非随请求取消
	kaCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	kaCh, err := lease.KeepAlive(kaCtx, id)
	if err != nil {
		cancel()
		logx.Errorf("snowflake: worker_id=%s 租约续期建立失败: %v", key, err)
		return
	}
	a.kaDone = make(chan struct{})
	go func() {
		defer cancel()
		defer close(a.kaDone)
		for range kaCh { // 消费续期响应;通道关闭 = 租约失效
			// 租约续期事件;失效时 etcd 侧 key 过期自动释放
		}
		logx.Errorf("snowflake: worker_id 租约失效,槽位 %s 已释放,请检查 etcd 连接", key)
	}()
}

// Release 主动释放(优雅退出调用):撤销租约,槽位立即可被其他实例抢占。
func (a *EtcdAllocator) Release(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leaseID == 0 {
		return nil
	}
	_, err := a.client.Revoke(ctx, a.leaseID)
	a.leaseID = 0
	if err != nil {
		return fmt.Errorf("snowflake: 释放租约失败: %w", err)
	}
	return nil
}

// Close 关闭底层 etcd client。
func (a *EtcdAllocator) Close() error {
	if a.kaDone != nil {
		<-a.kaDone
	}
	return a.client.Close()
}

// NewAuto 自动分配 worker_id:etcd 优先,不可用退化 MICRO_WORKER_ID 环境变量。
// 返回发号器与释放函数(进程优雅退出时调用,槽位立即让出)。
// 两个来源都不可用时返回 ErrNoWorkerSource——冲突不允许静默(不随机兜底)。
func NewAuto(ctx context.Context, etcdEndpoints []string) (*Node, func(), error) {
	if len(etcdEndpoints) > 0 {
		alloc, err := NewEtcdAllocator(etcdEndpoints, DefaultPrefix, DefaultLeaseTTL)
		if err == nil {
			// 探测+抢占限 5s:etcd 不可达时快速退化环境变量,不拖垮启动
			acqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			id, aerr := alloc.Acquire(acqCtx)
			cancel()
			if aerr == nil {
				node, nerr := New(id)
				if nerr != nil {
					_ = alloc.Release(context.WithoutCancel(ctx))
					_ = alloc.Close()
					return nil, nil, nerr
				}
				cleanup := func() {
					relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
					defer cancel()
					_ = alloc.Release(relCtx)
					_ = alloc.Close()
				}
				return node, cleanup, nil
			}
			logx.Errorf("snowflake: etcd 分配 worker_id 失败(%v),退化环境变量 %s", aerr, EnvWorkerID)
			_ = alloc.Close()
		} else {
			logx.Errorf("snowflake: 创建 etcd 分配器失败(%v),退化环境变量 %s", err, EnvWorkerID)
		}
	}

	raw := os.Getenv(EnvWorkerID)
	if raw == "" {
		return nil, nil, ErrNoWorkerSource
	}
	id, err := ParseWorkerID(raw)
	if err != nil {
		return nil, nil, err
	}
	node, err := New(id)
	if err != nil {
		return nil, nil, err
	}
	// 环境变量模式无租约可释放,返回空操作保持调用方接口一致
	return node, func() {}, nil
}
