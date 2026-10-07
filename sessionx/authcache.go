package sessionx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// auth_cache(02 §9.5):identity 将"角色→权限码"快照写入 Redis DB4,
// BFF 每请求一次 GET(+0.3~0.5ms),降权即时生效;
// 本地 LRU 兜底在 authz 包(fail-open 窗口)。

// ErrAuthCacheMiss auth_cache 无记录(未写入/已过期)。
var ErrAuthCacheMiss = errors.New("sessionx: auth_cache 无记录")

// AuthSnapshot uid 维度的权限快照。
type AuthSnapshot struct {
	Roles     []string `json:"roles"`
	Perms     []string `json:"perms"`
	DataScope string   `json:"data_scope"`
	TenantID  int64    `json:"tenant_id"`
}

// HasPerm 是否持有权限码(RBAC 判定)。
func (s *AuthSnapshot) HasPerm(code string) bool {
	if s == nil {
		return false
	}
	for _, p := range s.Perms {
		if p == code {
			return true
		}
	}
	return false
}

// PutAuthCache 写入/刷新权限快照(identity 登录、角色变更后调用;ttl 建议与会话寿命同阶)。
func (s *Store) PutAuthCache(ctx context.Context, uid int64, snap *AuthSnapshot, ttl time.Duration) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("sessionx: 序列化 auth_cache 失败: %w", err)
	}
	if ttl <= 0 {
		ttl = s.sessionTTL
	}
	return s.rdb.Set(ctx, s.key("auth_cache:", fmt.Sprintf("%d", uid)), raw, ttl).Err()
}

// GetAuthCache 读权限快照(BFF 每请求调用;无记录返回 ErrAuthCacheMiss)。
func (s *Store) GetAuthCache(ctx context.Context, uid int64) (*AuthSnapshot, error) {
	raw, err := s.rdb.Get(ctx, s.key("auth_cache:", fmt.Sprintf("%d", uid))).Result()
	if err != nil {
		if isNilErr(err) {
			return nil, ErrAuthCacheMiss
		}
		return nil, fmt.Errorf("sessionx: 读 auth_cache 失败: %w", err)
	}
	var snap AuthSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, fmt.Errorf("sessionx: 解析 auth_cache 失败: %w", err)
	}
	return &snap, nil
}

// DeleteAuthCache 即时失效(角色变更/封禁;BFF fail-open 窗口 ≤ LRU TTL)。
func (s *Store) DeleteAuthCache(ctx context.Context, uid int64) error {
	return s.rdb.Del(ctx, s.key("auth_cache:", fmt.Sprintf("%d", uid))).Err()
}

func isNilErr(err error) bool { return errors.Is(err, redis.Nil) }
