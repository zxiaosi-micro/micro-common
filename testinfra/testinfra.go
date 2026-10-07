// Package testinfra 实现单测中间件环境的三级解析(02 §16):
//
//	① MICRO_TEST_* 环境变量(CI 显式注入,最快最稳)
//	→ ② 本机 dev 中间件(compose 默认端口 + MICRO_DEV_* 口令,本地秒连)
//	→ ③ testcontainers 起容器(无本地中间件但机器有 Docker,可重复)
//	→ ④ 都不可用 → t.Skip(不阻塞单测运行)
//
// 服务仓的单测统一经本包取连接参数;测试自身不感知环境差异。
// 命名口径:S1-11 任务要求"CI 配 TDengine 容器作业强制全量"——
// micro-common 的 CI integration 作业含 tdengine 服务容器,
// 平台业务服务上线后沿用同一作业模式(TDengine 链路测试全量强制)。
package testinfra

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// dev 端口(02 §5.3:中间件宿主机端口 = 官方默认 + 20000)。
const (
	devMySQLPort    = "23306"
	devRedisPort    = "26379"
	devEtcdPort     = "22379"
	devKafkaPort    = "29092"
	devTDenginePort = "26041"
)

// Level 环境解析级别(排障/CI 报告可见)。
type Level string

const (
	LevelEnv       Level = "env"       // ① MICRO_TEST_* 注入
	LevelLocal     Level = "local"     // ② 本机 dev 中间件
	LevelContainer Level = "container" // ③ testcontainers
)

// Resource 一次解析出的中间件资源。
type Resource struct {
	// Level 实际命中的级别。
	Level Level
	// Cleanup 释放资源;容器场景为空操作——同一测试二进制内的容器进程级共享,
	// 由 testcontainers ryuk 在进程退出时统一回收。
	Cleanup func()
}

// —— 容器进程级共享:同一测试二进制内同类容器只起一次(ryuk 兜底回收) ——

var (
	containerMySQLOnce   sync.Once
	containerMySQLResult struct {
		dsn string
		err error
	}
	containerRedisOnce   sync.Once
	containerRedisResult struct {
		conn RedisConn
		err  error
	}
	containerEtcdOnce   sync.Once
	containerEtcdResult struct {
		endpoints []string
		err       error
	}
	containerKafkaOnce   sync.Once
	containerKafkaResult struct {
		brokers []string
		err     error
	}
	containerTDOnce   sync.Once
	containerTDResult struct {
		conn TDengineConn
		err  error
	}
)

// —— 通用探活 ——

func dialAlive(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// —— MySQL ——

// MySQL 返回测试用 MySQL DSN。
// 一级 MICRO_TEST_MYSQL_DSN;二级本机 127.0.0.1:23306(root + MICRO_DEV_MYSQL_PW);
// 三级 testcontainers mysql:8;四级 skip。
func MySQL(t *testing.T) (string, Resource) {
	t.Helper()
	if dsn := os.Getenv("MICRO_TEST_MYSQL_DSN"); dsn != "" {
		return dsn, Resource{Level: LevelEnv, Cleanup: func() {}}
	}
	local := fmt.Sprintf("127.0.0.1:%s", devMySQLPort)
	if dialAlive(local, 300*time.Millisecond) {
		pw := os.Getenv("MICRO_DEV_MYSQL_PW")
		dsn := fmt.Sprintf("root:%s@tcp(%s)/test?charset=utf8mb4&parseTime=true&loc=Local", pw, local)
		if pingMySQL(dsn) {
			return dsn, Resource{Level: LevelLocal, Cleanup: func() {}}
		}
	}
	if testing.Short() {
		t.Skip("testinfra: -short 模式跳过容器级测试(单测作业口径)")
	}
	t.Logf("testinfra: MySQL 走 testcontainers(本机 %s 不可用)", local)
	containerMySQLOnce.Do(func() {
		var cleanup func()
		containerMySQLResult.dsn, cleanup, containerMySQLResult.err = containerMySQL(t)
		_ = cleanup // 共享容器由 ryuk 进程退出时回收
	})
	if containerMySQLResult.err != nil {
		t.Skipf("testinfra: MySQL 三级解析全部失败(%v),跳过", containerMySQLResult.err)
	}
	return containerMySQLResult.dsn, Resource{Level: LevelContainer, Cleanup: func() {}}
}

func pingMySQL(dsn string) bool {
	db, err := openMySQL(dsn)
	if err != nil {
		return false
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return db.PingContext(ctx) == nil
}

// —— Redis ——

// RedisConn 测试用 Redis 连接参数(DB 划分见 02 §6.3,会话中心固定 DB4)。
type RedisConn struct {
	Addr     string
	Password string
	DB       int
}

// Redis 返回测试用 Redis 连接参数。
// 一级 MICRO_TEST_REDIS_ADDR(+REDIS_PASS/REDIS_DB);二级本机 127.0.0.1:26379;
// 三级 testcontainers redis:7;四级 skip。
func Redis(t *testing.T) (RedisConn, Resource) {
	t.Helper()
	if addr := os.Getenv("MICRO_TEST_REDIS_ADDR"); addr != "" {
		db, _ := strconv.Atoi(os.Getenv("MICRO_TEST_REDIS_DB"))
		return RedisConn{Addr: addr, Password: os.Getenv("MICRO_TEST_REDIS_PASS"), DB: db},
			Resource{Level: LevelEnv, Cleanup: func() {}}
	}
	local := fmt.Sprintf("127.0.0.1:%s", devRedisPort)
	if dialAlive(local, 300*time.Millisecond) {
		conn := RedisConn{Addr: local, Password: os.Getenv("MICRO_DEV_REDIS_PW")}
		if pingRedis(conn) {
			return conn, Resource{Level: LevelLocal, Cleanup: func() {}}
		}
	}
	if testing.Short() {
		t.Skip("testinfra: -short 模式跳过容器级测试(单测作业口径)")
	}
	t.Logf("testinfra: Redis 走 testcontainers(本机 %s 不可用)", local)
	containerRedisOnce.Do(func() {
		var cleanup func()
		containerRedisResult.conn, cleanup, containerRedisResult.err = containerRedis(t)
		_ = cleanup
	})
	if containerRedisResult.err != nil {
		t.Skipf("testinfra: Redis 三级解析全部失败(%v),跳过", containerRedisResult.err)
	}
	return containerRedisResult.conn, Resource{Level: LevelContainer, Cleanup: func() {}}
}

func pingRedis(c RedisConn) bool {
	client := redis.NewClient(&redis.Options{Addr: c.Addr, Password: c.Password, DialTimeout: time.Second})
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return client.Ping(ctx).Err() == nil
}

// —— etcd ——

// Etcd 返回测试用 etcd endpoints。
// 一级 MICRO_TEST_ETCD_ENDPOINTS(逗号分隔);二级本机 127.0.0.1:22379;
// 三级 testcontainers etcd;四级 skip。
func Etcd(t *testing.T) ([]string, Resource) {
	t.Helper()
	if eps := os.Getenv("MICRO_TEST_ETCD_ENDPOINTS"); eps != "" {
		return strings.Split(eps, ","), Resource{Level: LevelEnv, Cleanup: func() {}}
	}
	local := []string{fmt.Sprintf("127.0.0.1:%s", devEtcdPort)}
	if dialAlive(local[0], 300*time.Millisecond) {
		return local, Resource{Level: LevelLocal, Cleanup: func() {}}
	}
	if testing.Short() {
		t.Skip("testinfra: -short 模式跳过容器级测试(单测作业口径)")
	}
	t.Logf("testinfra: etcd 走 testcontainers(本机 %s 不可用)", local[0])
	containerEtcdOnce.Do(func() {
		var cleanup func()
		containerEtcdResult.endpoints, cleanup, containerEtcdResult.err = containerEtcd(t)
		_ = cleanup
	})
	if containerEtcdResult.err != nil {
		t.Skipf("testinfra: etcd 三级解析全部失败(%v),跳过", containerEtcdResult.err)
	}
	return containerEtcdResult.endpoints, Resource{Level: LevelContainer, Cleanup: func() {}}
}

// —— Kafka ——

// Kafka 返回测试用 Kafka brokers。
// 一级 MICRO_TEST_KAFKA_BROKERS(逗号分隔);二级本机 127.0.0.1:29092;
// 三级 testcontainers kafka(KRaft);四级 skip。
func Kafka(t *testing.T) ([]string, Resource) {
	t.Helper()
	if brokers := os.Getenv("MICRO_TEST_KAFKA_BROKERS"); brokers != "" {
		return strings.Split(brokers, ","), Resource{Level: LevelEnv, Cleanup: func() {}}
	}
	local := []string{fmt.Sprintf("127.0.0.1:%s", devKafkaPort)}
	if dialAlive(local[0], 300*time.Millisecond) {
		return local, Resource{Level: LevelLocal, Cleanup: func() {}}
	}
	if testing.Short() {
		t.Skip("testinfra: -short 模式跳过容器级测试(单测作业口径)")
	}
	t.Logf("testinfra: Kafka 走 testcontainers(本机 %s 不可用)", local[0])
	containerKafkaOnce.Do(func() {
		var cleanup func()
		containerKafkaResult.brokers, cleanup, containerKafkaResult.err = containerKafka(t)
		_ = cleanup
	})
	if containerKafkaResult.err != nil {
		t.Skipf("testinfra: Kafka 三级解析全部失败(%v),跳过", containerKafkaResult.err)
	}
	return containerKafkaResult.brokers, Resource{Level: LevelContainer, Cleanup: func() {}}
}

// —— TDengine ——

// TDengine 返回测试用 TDengine REST 连接参数(6041 REST 必须 Basic 认证,E7)。
// 一级 MICRO_TEST_TDENGINE_ADDR(+USER/PASS);二级本机 127.0.0.1:26041(root/taosdata
// 或 MICRO_DEV_TDENGINE_PW);三级 testcontainers tdengine;四级 skip。
// S1 阶段仅 CI 冒烟(容器作业强制全量口径);业务服务遥测测试自 S6 起消费。
func TDengine(t *testing.T) (TDengineConn, Resource) {
	t.Helper()
	if addr := os.Getenv("MICRO_TEST_TDENGINE_ADDR"); addr != "" {
		return TDengineConn{
			Addr: addr,
			User: envOr("MICRO_TEST_TDENGINE_USER", "root"),
			Pass: envOr("MICRO_TEST_TDENGINE_PASS", "taosdata"),
		}, Resource{Level: LevelEnv, Cleanup: func() {}}
	}
	local := fmt.Sprintf("127.0.0.1:%s", devTDenginePort)
	if dialAlive(local, 300*time.Millisecond) {
		conn := TDengineConn{
			Addr: local,
			User: "root",
			Pass: envOr("MICRO_DEV_TDENGINE_PW", "taosdata"),
		}
		if pingTDengine(conn) {
			return conn, Resource{Level: LevelLocal, Cleanup: func() {}}
		}
	}
	if testing.Short() {
		t.Skip("testinfra: -short 模式跳过容器级测试(单测作业口径)")
	}
	t.Logf("testinfra: TDengine 走 testcontainers(本机 %s 不可用)", local)
	containerTDOnce.Do(func() {
		var cleanup func()
		containerTDResult.conn, cleanup, containerTDResult.err = containerTDengine(t)
		_ = cleanup
	})
	if containerTDResult.err != nil {
		t.Skipf("testinfra: TDengine 三级解析全部失败(%v),跳过", containerTDResult.err)
	}
	return containerTDResult.conn, Resource{Level: LevelContainer, Cleanup: func() {}}
}

// TDengineConn TDengine REST 连接参数。
type TDengineConn struct {
	Addr string // host:port(6041)
	User string
	Pass string
}

// BasicAuth REST 请求的 Basic 认证头值(E7:401 易误判空结果,必须带认证)。
func (c TDengineConn) BasicAuth() string {
	return fmt.Sprintf("%s:%s", c.User, c.Pass)
}

// URL REST 端点 URL。
func (c TDengineConn) URL() string {
	return fmt.Sprintf("http://%s/rest/sql", c.Addr)
}

func pingTDengine(c TDengineConn) bool {
	client := newTDengineClient(2 * time.Second)
	return client.ping(c) == nil
}
