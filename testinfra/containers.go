package testinfra

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // database/sql mysql 驱动
	"github.com/testcontainers/testcontainers-go"
	kafkamod "github.com/testcontainers/testcontainers-go/modules/kafka"
	"github.com/testcontainers/testcontainers-go/wait"
)

// container* 系列:三级解析的第③级。机器无 Docker 时返回错误,调用方 t.Skip。

func dockerAvailable() bool {
	// testcontainers 每次探测 ryuk/docker 环境开销大,这里用 provider check 简化
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return false
	}
	_ = provider.Close()
	_ = ctx
	return true
}

func genericStart(t *testing.T, req testcontainers.ContainerRequest, ports []string, ready func(host string, portMap map[string]string) error, maxWait time.Duration) (testcontainers.Container, string, map[string]string, error) {
	t.Helper()
	if !dockerAvailable() {
		return nil, "", nil, errors.New("docker 不可用")
	}
	ctx := context.Background()
	req.WaitingFor = wait.ForListeningPort(firstPort(ports)).WithStartupTimeout(maxWait)
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("启动容器失败: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, "", nil, fmt.Errorf("取容器 host 失败: %w", err)
	}
	portMap := make(map[string]string, len(ports))
	for _, p := range ports {
		mp, err := c.MappedPort(ctx, p)
		if err != nil {
			_ = c.Terminate(ctx)
			return nil, "", nil, fmt.Errorf("取映射端口 %s 失败: %w", p, err)
		}
		portMap[p] = mp.Port()
	}
	// 就绪探测(服务真实可连),失败重试至 maxWait
	deadline := time.Now().Add(maxWait)
	for {
		err := ready(host, portMap)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Terminate(ctx)
			return nil, "", nil, fmt.Errorf("容器就绪探测超时: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return c, host, portMap, nil
}

func firstPort(ports []string) string {
	if len(ports) == 0 {
		return ""
	}
	return ports[0]
}

func terminateFn(c testcontainers.Container) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	}
}

func containerMySQL(t *testing.T) (string, func(), error) {
	c, host, pm, err := genericStart(t, testcontainers.ContainerRequest{
		Image:        "mysql:8.0",
		ExposedPorts: []string{"3306/tcp"},
		Env: map[string]string{
			"MYSQL_ROOT_PASSWORD": "microtest",
			"MYSQL_DATABASE":      "test",
		},
		AutoRemove: true,
	}, []string{"3306/tcp"}, func(host string, pm map[string]string) error {
		dsn := fmt.Sprintf("root:microtest@tcp(%s:%s)/test?charset=utf8mb4&parseTime=true&loc=Local", host, pm["3306/tcp"])
		if !pingMySQL(dsn) {
			return errors.New("mysql ping 失败")
		}
		return nil
	}, 3*time.Minute)
	if err != nil {
		return "", nil, err
	}
	dsn := fmt.Sprintf("root:microtest@tcp(%s:%s)/test?charset=utf8mb4&parseTime=true&loc=Local", host, pm["3306/tcp"])
	return dsn, terminateFn(c), nil
}

func containerRedis(t *testing.T) (RedisConn, func(), error) {
	c, host, pm, err := genericStart(t, testcontainers.ContainerRequest{
		Image:        "redis:7",
		ExposedPorts: []string{"6379/tcp"},
		AutoRemove:   true,
	}, []string{"6379/tcp"}, func(host string, pm map[string]string) error {
		if !pingRedis(RedisConn{Addr: fmt.Sprintf("%s:%s", host, pm["6379/tcp"])}) {
			return errors.New("redis ping 失败")
		}
		return nil
	}, time.Minute)
	if err != nil {
		return RedisConn{}, nil, err
	}
	return RedisConn{Addr: fmt.Sprintf("%s:%s", host, pm["6379/tcp"])}, terminateFn(c), nil
}

func containerEtcd(t *testing.T) ([]string, func(), error) {
	c, host, pm, err := genericStart(t, testcontainers.ContainerRequest{
		Image: "gcr.io/etcd-development/etcd:v3.5.14",
		Cmd: []string{
			"etcd",
			"--listen-client-urls=http://0.0.0.0:2379",
			"--advertise-client-urls=http://0.0.0.0:2379",
			"--listen-peer-urls=http://0.0.0.0:2380",
			"--initial-advertise-peer-urls=http://0.0.0.0:2380",
			"--initial-cluster=default=http://0.0.0.0:2380",
		},
		ExposedPorts: []string{"2379/tcp"},
		AutoRemove:   true,
	}, []string{"2379/tcp"}, func(host string, pm map[string]string) error {
		if !dialAlive(fmt.Sprintf("%s:%s", host, pm["2379/tcp"]), time.Second) {
			return errors.New("etcd 端口未就绪")
		}
		return nil
	}, 2*time.Minute)
	if err != nil {
		return nil, nil, err
	}
	return []string{fmt.Sprintf("%s:%s", host, pm["2379/tcp"])}, terminateFn(c), nil
}

func containerKafka(t *testing.T) ([]string, func(), error) {
	if !dockerAvailable() {
		return nil, nil, errors.New("docker 不可用")
	}
	// 官方 kafka module:KRaft 单机 + advertised listener 自动指向映射端口(E3)
	c, err := kafkamod.Run(context.Background(), "confluentinc/cp-kafka:7.6.1")
	if err != nil {
		if c != nil {
			_ = c.Terminate(context.Background())
		}
		return nil, nil, fmt.Errorf("启动 kafka 容器失败: %w", err)
	}
	brokers, err := c.Brokers(context.Background())
	if err != nil {
		_ = c.Terminate(context.Background())
		return nil, nil, fmt.Errorf("取 kafka brokers 失败: %w", err)
	}
	return brokers, terminateFn(c), nil
}

func containerTDengine(t *testing.T) (TDengineConn, func(), error) {
	c, host, pm, err := genericStart(t, testcontainers.ContainerRequest{
		Image: "tdengine/tdengine:3.3.2.0",
		Env: map[string]string{
			"TAOS_FQDN": "localhost",
		},
		ExposedPorts: []string{"6041/tcp"},
		AutoRemove:   true,
	}, []string{"6041/tcp"}, func(host string, pm map[string]string) error {
		if !pingTDengine(TDengineConn{Addr: fmt.Sprintf("%s:%s", host, pm["6041/tcp"]), User: "root", Pass: "taosdata"}) {
			return errors.New("tdengine REST 未就绪")
		}
		return nil
	}, 3*time.Minute)
	if err != nil {
		return TDengineConn{}, nil, err
	}
	return TDengineConn{
		Addr: fmt.Sprintf("%s:%s", host, pm["6041/tcp"]),
		User: "root",
		Pass: "taosdata",
	}, terminateFn(c), nil
}

// —— 小工具:探活与 HTTP 客户端 ——

func openMySQL(dsn string) (*sql.DB, error) {
	return sql.Open("mysql", dsn)
}

// newTDengineClient TDengine REST 探活客户端(必须 Basic 认证,E7)。
type tdengineClient struct {
	client *http.Client
}

func newTDengineClient(timeout time.Duration) *tdengineClient {
	return &tdengineClient{client: &http.Client{Timeout: timeout}}
}

func (c *tdengineClient) ping(conn TDengineConn) error {
	req, err := http.NewRequest(http.MethodPost, conn.URL(), strings.NewReader("show databases"))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(conn.BasicAuth())))
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("testinfra: TDengine 认证失败(401),检查口令(E7)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("testinfra: TDengine REST 状态 %d", resp.StatusCode)
	}
	return nil
}
