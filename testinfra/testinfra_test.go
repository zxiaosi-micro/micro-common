package testinfra

import (
	"testing"
)

func TestEnvLevelMySQL(t *testing.T) {
	t.Setenv("MICRO_TEST_MYSQL_DSN", "root:pw@tcp(10.1.1.1:3306)/test")
	dsn, res := MySQL(t)
	if dsn != "root:pw@tcp(10.1.1.1:3306)/test" || res.Level != LevelEnv {
		t.Fatalf("一级解析失败: %s %+v", dsn, res.Level)
	}
}

func TestEnvLevelRedis(t *testing.T) {
	t.Setenv("MICRO_TEST_REDIS_ADDR", "10.1.1.2:6379")
	t.Setenv("MICRO_TEST_REDIS_PASS", "secret")
	t.Setenv("MICRO_TEST_REDIS_DB", "4")
	conn, res := Redis(t)
	if conn.Addr != "10.1.1.2:6379" || conn.Password != "secret" || conn.DB != 4 || res.Level != LevelEnv {
		t.Fatalf("一级解析失败: %+v %+v", conn, res.Level)
	}
}

func TestEnvLevelEtcdKafka(t *testing.T) {
	t.Setenv("MICRO_TEST_ETCD_ENDPOINTS", "e1:2379,e2:2379")
	eps, res := Etcd(t)
	if len(eps) != 2 || eps[0] != "e1:2379" || res.Level != LevelEnv {
		t.Fatalf("etcd 一级解析失败: %v", eps)
	}
	t.Setenv("MICRO_TEST_KAFKA_BROKERS", "b1:9092")
	brokers, res2 := Kafka(t)
	if len(brokers) != 1 || brokers[0] != "b1:9092" || res2.Level != LevelEnv {
		t.Fatalf("kafka 一级解析失败: %v", brokers)
	}
}

func TestEnvLevelTDengine(t *testing.T) {
	t.Setenv("MICRO_TEST_TDENGINE_ADDR", "10.1.1.3:6041")
	t.Setenv("MICRO_TEST_TDENGINE_USER", "root")
	t.Setenv("MICRO_TEST_TDENGINE_PASS", "custom")
	conn, res := TDengine(t)
	if conn.Addr != "10.1.1.3:6041" || conn.BasicAuth() != "root:custom" || res.Level != LevelEnv {
		t.Fatalf("TDengine 一级解析失败: %+v", conn)
	}
	if conn.URL() != "http://10.1.1.3:6041/rest/sql" {
		t.Fatalf("URL = %q", conn.URL())
	}
}

// TestResolveRealMySQL 冒烟:三级解析在真实环境下应给出可用连接或显式 Skip。
// 有本机/CI 中间件或 Docker 时全量跑;都没有则 Skip(不阻塞单测)。
func TestResolveRealMySQL(t *testing.T) {
	dsn, res := MySQL(t)
	t.Logf("MySQL 解析级别=%s dsn=%s", res.Level, maskDSN(dsn))
	db, err := openMySQL(dsn)
	if err != nil {
		t.Fatalf("打开连接失败: %v", err)
	}
	defer func() { _ = db.Close() }()
}

func maskDSN(dsn string) string {
	// 只留端口与库名,避免口令出现在测试日志
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == ':' && i < 12 {
			return "root:***" + dsn[minInt(i+8, len(dsn)):]
		}
	}
	return dsn
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestResolveRealTDengine TDengine 容器作业冒烟(S1-11:CI 强制全量口径)。
// integration 作业带 tdengine 服务容器时验证 Basic 认证可用(E7)。
func TestResolveRealTDengine(t *testing.T) {
	conn, res := TDengine(t)
	t.Logf("TDengine 解析级别=%s addr=%s", res.Level, conn.Addr)
	if !pingTDengine(conn) {
		t.Fatal("TDengine REST 探活失败(检查 Basic 认证,E7)")
	}
}
