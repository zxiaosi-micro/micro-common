// Package sessionx 实现 Redis 会话中心(identity 服务唯一读写方,02 §9.5)。
//
// Redis DB4 键位契约:
//
//	sess:{sid}                HASH   {uid, client, created_at}     会话本体,TTL=会话寿命
//	uid_sessions:{uid}        ZSET   member=sid score=created_at   用户全部会话
//	mutex:{uid}:{client}      STRING sid                          同端互斥标记(QQ 模式)
//	tomb:{sid}                STRING reason                        会话墓碑(kicked/logout/revoked)
//	rt:{rtid}                 STRING sid                          refresh token 索引
//	rt_tomb:{rtid}            STRING uid                          refresh 重用墓碑
//	stepup:{sid}              STRING level                         二级认证级别,TTL=step-up 窗口
//	login_fail:{ident}        STRING count                         登录失败计数
//	locked:{ident}            STRING 1                             账号锁定标记
//
// 直用 go-redis(v9)而非 go-zero redis 封装:DB4 独立选库 + 多段 Lua 原子语义。
package sessionx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 会话注销原因(墓碑值,authz 据此映射 10402/10403)。
const (
	ReasonKicked  = "kicked"  // 互斥/被踢
	ReasonLogout  = "logout"  // 主动登出
	ReasonRevoked = "revoked" // 重用全端注销/封禁
)

// 会话寿命默认值:access 30min 靠 JWT;会话本体寿命随 refresh 线滚动。
const (
	DefaultSessionTTL = 7 * 24 * time.Hour
	DefaultRefreshTTL = 7 * 24 * time.Hour
	DefaultStepUpTTL  = 5 * time.Minute
)

var (
	// ErrSessionNotFound 会话不存在(已过期/已注销/未创建)。
	ErrSessionNotFound = errors.New("sessionx: 会话不存在")
	// ErrRefreshInvalid refresh token 无效或过期。
	ErrRefreshInvalid = errors.New("sessionx: refresh token 无效")
	// ErrRefreshReuse refresh token 被重用——已触发全端注销并告警。
	ErrRefreshReuse = errors.New("sessionx: refresh token 重用,已全端注销")
)

// Conf 会话中心配置(由 identity 服务 yaml 注入)。
type Conf struct {
	Addr       string        `json:",default=127.0.0.1:26379"`
	Password   string        `json:",optional"`
	DB         int           `json:",default=4"` // 会话中心固定 DB4(02 §6.3)
	SessionTTL time.Duration `json:",default=604800s"`
	RefreshTTL time.Duration `json:",default=604800s"`
	StepUpTTL  time.Duration `json:",default=300s"`
	Prefix     string        `json:",optional"` // 键前缀(多环境隔离用)
}

// Session 会话快照。
type Session struct {
	SID       string
	UID       int64
	Client    string
	CreatedAt time.Time
}

// Store 会话中心存储。
type Store struct {
	rdb        *redis.Client
	prefix     string
	sessionTTL time.Duration
	refreshTTL time.Duration
	stepUpTTL  time.Duration
}

// New 构造并探活(会话中心是 identity 硬依赖,启动期暴露连不通)。
func New(c Conf) (*Store, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        c.Addr,
		Password:    c.Password,
		DB:          c.DB,
		DialTimeout: 3 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("sessionx: redis ping 失败: %w", err)
	}
	return NewFromClient(rdb, c), nil
}

// NewFromClient 以现成 client 构造(测试/复用连接池)。
func NewFromClient(rdb *redis.Client, c Conf) *Store {
	stepUpTTL := c.StepUpTTL
	if stepUpTTL <= 0 {
		stepUpTTL = DefaultStepUpTTL
	}
	sessionTTL := c.SessionTTL
	if sessionTTL <= 0 {
		sessionTTL = DefaultSessionTTL
	}
	refreshTTL := c.RefreshTTL
	if refreshTTL <= 0 {
		refreshTTL = DefaultRefreshTTL
	}
	return &Store{
		rdb:        rdb,
		prefix:     c.Prefix,
		sessionTTL: sessionTTL,
		refreshTTL: refreshTTL,
		stepUpTTL:  stepUpTTL,
	}
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.rdb.Close() }

// Ping 探活(健康检查用)。
func (s *Store) Ping(ctx context.Context) error { return s.rdb.Ping(ctx).Err() }

func (s *Store) key(parts ...string) string {
	out := s.prefix
	for _, p := range parts {
		out += p
	}
	return out
}

// newSID/newRTID 128bit 随机 hex(不可猜测)。
func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("sessionx: 生成随机令牌失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// —— 同端互斥:Lua 原子交换 mutex,返回被顶会话 ——

const mutexSwapScript = `
local old = redis.call('GET', KEYS[1])
redis.call('SET', KEYS[1], ARGV[1], 'EX', tonumber(ARGV[2]))
return old or ''
`

// compareDeleteScript mutex 清理仅当仍指向本会话(防误删新会话标记)。
const compareDeleteScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`

// Create 创建会话;同端互斥踢旧(QQ 模式:同 client 只留最新,跨 client 共存),
// 返回新会话与被踢下线的会话列表。
func (s *Store) Create(ctx context.Context, uid int64, client string) (*Session, []string, error) {
	sid, err := newToken()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	sess := &Session{SID: sid, UID: uid, Client: client, CreatedAt: now}

	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, s.key("sess:", sid), map[string]any{
		"uid":        uid,
		"client":     client,
		"created_at": now.UnixMilli(),
	})
	pipe.Expire(ctx, s.key("sess:", sid), s.sessionTTL)
	pipe.ZAdd(ctx, s.key("uid_sessions:", fmt.Sprintf("%d", uid)), redis.Z{
		Score:  float64(now.UnixMilli()),
		Member: sid,
	})
	pipe.Expire(ctx, s.key("uid_sessions:", fmt.Sprintf("%d", uid)), s.sessionTTL+time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, nil, fmt.Errorf("sessionx: 写会话失败: %w", err)
	}

	// 互斥:原子交换 mutex,拿到被顶 sid 后补墓碑并清理其会话
	mutexKey := s.key("mutex:", fmt.Sprintf("%d", uid), ":", client)
	old := s.rdb.Eval(ctx, mutexSwapScript, []string{mutexKey},
		sid, int(s.sessionTTL/time.Second)).Val()
	var kicked []string
	if oldSid := old.(string); oldSid != "" && oldSid != sid {
		kicked = append(kicked, oldSid)
		s.writeTomb(ctx, oldSid, ReasonKicked, s.sessionTTL)
		s.removeSession(ctx, uid, oldSid, client)
	}
	return sess, kicked, nil
}

// Get 读会话;已注销/过期返回 ErrSessionNotFound。
func (s *Store) Get(ctx context.Context, sid string) (*Session, error) {
	m, err := s.rdb.HGetAll(ctx, s.key("sess:", sid)).Result()
	if err != nil {
		return nil, fmt.Errorf("sessionx: 读会话失败: %w", err)
	}
	if len(m) == 0 {
		return nil, ErrSessionNotFound
	}
	var uid int64
	_, _ = fmt.Sscan(m["uid"], &uid)
	created := time.Time{}
	if ms, ok := m["created_at"]; ok {
		var v int64
		_, _ = fmt.Sscan(ms, &v)
		created = time.UnixMilli(v)
	}
	return &Session{SID: sid, UID: uid, Client: m["client"], CreatedAt: created}, nil
}

// TombReason 查会话墓碑(会话不存在时,BFF 据此区分被顶/注销)。
// 返回空串表示无墓碑(自然过期或不存在)。
func (s *Store) TombReason(ctx context.Context, sid string) (string, error) {
	v, err := s.rdb.Get(ctx, s.key("tomb:", sid)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sessionx: 查墓碑失败: %w", err)
	}
	return v, nil
}

// Revoke 注销单个会话(reason ∈ Reason*)。
func (s *Store) Revoke(ctx context.Context, sid, reason string) error {
	sess, err := s.Get(ctx, sid)
	if errors.Is(err, ErrSessionNotFound) {
		return nil // 幂等
	}
	if err != nil {
		return err
	}
	s.writeTomb(ctx, sid, reason, s.sessionTTL)
	s.removeSession(ctx, sess.UID, sid, sess.Client)
	return nil
}

// RevokeClient 踢指定端全部会话;返回注销数。
func (s *Store) RevokeClient(ctx context.Context, uid int64, client string) (int, error) {
	sessions, err := s.ListByUID(ctx, uid)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sess := range sessions {
		if sess.Client != client {
			continue
		}
		s.writeTomb(ctx, sess.SID, ReasonKicked, s.sessionTTL)
		s.removeSession(ctx, uid, sess.SID, sess.Client)
		n++
	}
	return n, nil
}

// RevokeAll 全端注销(封禁/refresh 重用触发);返回注销数。
func (s *Store) RevokeAll(ctx context.Context, uid int64) (int, error) {
	sessions, err := s.ListByUID(ctx, uid)
	if err != nil {
		return 0, err
	}
	for _, sess := range sessions {
		s.writeTomb(ctx, sess.SID, ReasonRevoked, s.sessionTTL)
		s.removeSession(ctx, uid, sess.SID, sess.Client)
	}
	_ = s.rdb.Del(ctx, s.key("uid_sessions:", fmt.Sprintf("%d", uid))).Err()
	return len(sessions), nil
}

// ListByUID 会话管理页:在线会话列表(惰性清理 ZSET 中已过期成员)。
func (s *Store) ListByUID(ctx context.Context, uid int64) ([]Session, error) {
	zsetKey := s.key("uid_sessions:", fmt.Sprintf("%d", uid))
	sids, err := s.rdb.ZRange(ctx, zsetKey, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("sessionx: 读会话索引失败: %w", err)
	}
	if len(sids) == 0 {
		return nil, nil
	}
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(sids))
	for i, sid := range sids {
		cmds[i] = pipe.HGetAll(ctx, s.key("sess:", sid))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("sessionx: 批量读会话失败: %w", err)
	}
	out := make([]Session, 0, len(sids))
	dead := make([]any, 0, len(sids))
	for i, cmd := range cmds {
		m := cmd.Val()
		if len(m) == 0 {
			dead = append(dead, sids[i])
			continue
		}
		var uidVal int64
		_, _ = fmt.Sscan(m["uid"], &uidVal)
		var created time.Time
		if ms, ok := m["created_at"]; ok {
			var v int64
			_, _ = fmt.Sscan(ms, &v)
			created = time.UnixMilli(v)
		}
		out = append(out, Session{SID: sids[i], UID: uidVal, Client: m["client"], CreatedAt: created})
	}
	if len(dead) > 0 {
		_ = s.rdb.ZRem(ctx, zsetKey, dead...).Err()
	}
	return out, nil
}

// RaiseLevel step-up 二级认证:5min 窗口内该会话 auth_level=2。
// identity 随后重签 level=2 的 access token。
func (s *Store) RaiseLevel(ctx context.Context, sid string, level int) error {
	return s.rdb.Set(ctx, s.key("stepup:", sid), level, s.stepUpTTL).Err()
}

// EffectiveLevel 会话生效认证级别(1 普通;step-up 窗口内 2)。
func (s *Store) EffectiveLevel(ctx context.Context, sid string) (int, error) {
	if _, err := s.Get(ctx, sid); err != nil {
		return 0, err
	}
	v, err := s.rdb.Get(ctx, s.key("stepup:", sid)).Result()
	if errors.Is(err, redis.Nil) {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sessionx: 读 step-up 级别失败: %w", err)
	}
	var level int
	_, _ = fmt.Sscan(v, &level)
	if level < 1 {
		level = 1
	}
	return level, nil
}

func (s *Store) writeTomb(ctx context.Context, sid, reason string, ttl time.Duration) {
	_ = s.rdb.Set(ctx, s.key("tomb:", sid), reason, ttl).Err()
}

// removeSession 删会话本体 + ZSET 索引 + mutex 条件清理。
func (s *Store) removeSession(ctx context.Context, uid int64, sid, client string) {
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, s.key("sess:", sid))
	pipe.ZRem(ctx, s.key("uid_sessions:", fmt.Sprintf("%d", uid)), sid)
	pipe.Del(ctx, s.key("stepup:", sid))
	_, _ = pipe.Exec(ctx)
	if client != "" {
		s.rdb.Eval(ctx, compareDeleteScript,
			[]string{s.key("mutex:", fmt.Sprintf("%d", uid), ":", client)}, sid)
	}
}
