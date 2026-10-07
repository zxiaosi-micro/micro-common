package sessionx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// login_fail 锁定(02 §9.5 / S3-01 user.locked_until 同语义的 Redis 快路径):
//
//	RecordLoginFail 失败计数 +1(窗口内),达到阈值 → 锁定 locked:{ident} EX lockDur
//	CheckLocked 查锁定状态与剩余秒数(前端 10407 倒计时)
//	ClearLoginFails 登录成功清零
//
// ident 建议用 mobile_hash(uid 未登录前不可得)。

// lockScript 计数与锁定原子化。
const lockScript = `
local n = redis.call('INCR', KEYS[1])
if n == 1 then
	redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
end
if n >= tonumber(ARGV[1]) then
	redis.call('SET', KEYS[2], '1', 'PX', tonumber(ARGV[3]))
	redis.call('DEL', KEYS[1])
	return {'1', '0'}
end
local remain = tonumber(ARGV[1]) - n
return {'0', tostring(remain)}
`

var (
	// ErrLocked 账号处于锁定状态。
	ErrLocked = errors.New("sessionx: 账号已锁定")
)

// LockResult 一次登录失败记录的结果。
type LockResult struct {
	// Locked 本次失败是否触发锁定。
	Locked bool
	// Remaining 距锁定的剩余失败次数(已锁定为 0)。
	Remaining int
}

// RecordLoginFail 记录一次登录失败;达到 maxFails(窗口内)则锁定 lockDur。
func (s *Store) RecordLoginFail(ctx context.Context, ident string, maxFails int, failWindow, lockDur time.Duration) (LockResult, error) {
	if maxFails < 1 {
		maxFails = 1
	}
	res, err := s.rdb.Eval(ctx, lockScript,
		[]string{s.key("login_fail:", ident), s.key("locked:", ident)},
		maxFails, int(failWindow/time.Millisecond), int(lockDur/time.Millisecond)).Result()
	if err != nil {
		return LockResult{}, fmt.Errorf("sessionx: 记录登录失败失败: %w", err)
	}
	arr := res.([]any)
	locked := arr[0].(string) == "1"
	remaining, _ := strconv.Atoi(arr[1].(string))
	return LockResult{Locked: locked, Remaining: remaining}, nil
}

// LockedState 锁定状态:locked=true 时 remaining 为剩余锁定时长。
type LockedState struct {
	Locked    bool
	Remaining time.Duration
}

// CheckLocked 查锁定状态(登录入口与 BFF 提示倒计时用)。
// 用 PTTL(毫秒精度):TTL 秒级精度会把亚秒级 TTL 读成 0,误判为未锁。
func (s *Store) CheckLocked(ctx context.Context, ident string) (LockedState, error) {
	ttl, err := s.rdb.PTTL(ctx, s.key("locked:", ident)).Result()
	if err != nil {
		return LockedState{}, fmt.Errorf("sessionx: 查锁定状态失败: %w", err)
	}
	if ttl <= 0 {
		return LockedState{}, nil
	}
	return LockedState{Locked: true, Remaining: ttl}, nil
}

// ClearLoginFails 登录成功后清空失败计数。
func (s *Store) ClearLoginFails(ctx context.Context, ident string) error {
	return s.rdb.Del(ctx, s.key("login_fail:", ident), s.key("locked:", ident)).Err()
}

// IsLockedError 判定错误是否为锁定错误(identity 逻辑层映射 10407 用)。
func IsLockedError(err error) bool { return errors.Is(err, ErrLocked) }
