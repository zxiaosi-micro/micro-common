package snowflake

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	if _, err := New(-1); err == nil {
		t.Fatal("worker_id=-1 应拒绝")
	}
	if _, err := New(MaxWorker + 1); err == nil {
		t.Fatal("worker_id 越上界应拒绝")
	}
	if _, err := New(MaxWorker); err != nil {
		t.Fatalf("worker_id=%d 应合法: %v", MaxWorker, err)
	}
}

func TestNextIDLayoutAndMonotonic(t *testing.T) {
	n, _ := New(7)
	var prev int64
	for i := 0; i < 1000; i++ {
		id, err := n.NextID()
		if err != nil {
			t.Fatalf("发号失败: %v", err)
		}
		if id <= prev {
			t.Fatalf("ID 应回退: prev=%d cur=%d", prev, id)
		}
		prev = id
		// 位域布局:41bit 时间 | 10bit worker | 12bit seq
		ts, worker, seq := ParseID(id)
		if worker != 7 {
			t.Fatalf("worker = %d, want 7", worker)
		}
		if seq > MaxSeq {
			t.Fatalf("seq = %d 越界", seq)
		}
		if ts.Before(time.UnixMilli(Epoch)) {
			t.Fatalf("时间戳早于纪元: %v", ts)
		}
	}
}

func TestParseIDComponents(t *testing.T) {
	n, _ := New(123)
	id, _ := n.NextID()
	ts, worker, seq := ParseID(id)
	if worker != 123 || seq != 0 {
		t.Fatalf("parse = (%v,%d,%d)", ts, worker, seq)
	}
	if diff := time.Since(ts); diff < 0 || diff > 5*time.Second {
		t.Fatalf("时间戳偏离当前时间: %v", diff)
	}
}

func TestUniquenessUnderConcurrency(t *testing.T) {
	n, _ := New(1)
	const goroutines = 50
	const perG = 200

	ids := make([]int64, goroutines*perG)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				id, err := n.NextID()
				if err != nil {
					t.Errorf("并发发号失败: %v", err)
					return
				}
				ids[g*perG+i] = id
			}
		}(g)
	}
	wg.Wait()

	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("并发下出现重复 ID: %d", id)
		}
		seen[id] = struct{}{}
	}
}

func TestParseWorkerID(t *testing.T) {
	if v, err := ParseWorkerID("42"); err != nil || v != 42 {
		t.Fatalf("ParseWorkerID(42) = %d, %v", v, err)
	}
	if _, err := ParseWorkerID("abc"); err == nil {
		t.Fatal("非数字应报错")
	}
	if _, err := ParseWorkerID("9999"); err == nil {
		t.Fatal("越界应报错")
	}
}

func TestNewAutoEnvFallback(t *testing.T) {
	// etcd 不可达 → 退化环境变量
	t.Setenv(EnvWorkerID, "77")
	node, cleanup, err := NewAuto(context.Background(), []string{"127.0.0.1:1"})
	if err != nil {
		t.Fatalf("etcd 不可用应退化环境变量: %v", err)
	}
	defer cleanup()
	if node.WorkerID() != 77 {
		t.Fatalf("worker = %d, want 77", node.WorkerID())
	}
}

func TestNewAutoEnvInvalid(t *testing.T) {
	t.Setenv(EnvWorkerID, "not-a-number")
	if _, _, err := NewAuto(context.Background(), []string{"127.0.0.1:1"}); err == nil {
		t.Fatal("非法环境变量应报错")
	}
}

func TestNewAutoNoSource(t *testing.T) {
	// etcd 不可达 + 未设置环境变量 → ErrNoWorkerSource(冲突不静默,不随机)
	t.Setenv(EnvWorkerID, "")
	_, _, err := NewAuto(context.Background(), []string{"127.0.0.1:1"})
	if err != ErrNoWorkerSource {
		t.Fatalf("应返回 ErrNoWorkerSource, got %v", err)
	}
}
