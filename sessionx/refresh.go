package sessionx

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// refresh 轮换式令牌 + 重用墓碑(02 §9.5):
//
//	IssueRefresh 签发 rtid → rt:{rtid}=sid
//	RotateRefresh 消费 rtid(GETDEL 原子)→ 旧 rtid 落墓碑 rt_tomb:{rtid}=uid → 换发新 rtid
//	重用已消费的 rtid → 命中墓碑 → 该用户全端注销 + 返回 ErrRefreshReuse(告警由调用方接)
//
// GETDEL 需要 Redis ≥ 6.2(dev compose 与生产口径为 Redis 7)。

// getdelScript 原子消费:取值并删除,防止并发重放轮换。
const getdelScript = `
local v = redis.call('GET', KEYS[1])
if v then
	redis.call('DEL', KEYS[1])
end
return v or ''
`

// IssueRefresh 为会话签发 refresh token(rtid)。
func (s *Store) IssueRefresh(ctx context.Context, uid int64, sid string) (string, error) {
	rtid, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.rdb.Set(ctx, s.key("rt:", rtid), sid, s.refreshTTL).Err(); err != nil {
		return "", fmt.Errorf("sessionx: 写 refresh token 失败: %w", err)
	}
	return rtid, nil
}

// RotateRefresh 轮换 refresh token:返回新 rtid 与所属会话。
// 重用检测:rtid 已消费(墓碑命中)→ 全端注销并返回 ErrRefreshReuse。
func (s *Store) RotateRefresh(ctx context.Context, rtid string) (newRTID, sid string, err error) {
	// 1. 重用检测(墓碑先行:已消费过的 rtid 一定有墓碑)
	uidStr, err := s.rdb.Get(ctx, s.key("rt_tomb:", rtid)).Result()
	switch {
	case err == nil:
		var uid int64
		_, _ = fmt.Sscan(uidStr, &uid)
		if _, rerr := s.RevokeAll(ctx, uid); rerr != nil {
			return "", "", fmt.Errorf("sessionx: 重用触发全端注销失败: %w", rerr)
		}
		return "", "", ErrRefreshReuse
	case !errors.Is(err, redis.Nil):
		return "", "", fmt.Errorf("sessionx: 查 refresh 墓碑失败: %w", err)
	}

	// 2. 原子消费 rt:{rtid}
	sidVal := s.rdb.Eval(ctx, getdelScript, []string{s.key("rt:", rtid)}).Val()
	sid, _ = sidVal.(string)
	if sid == "" {
		return "", "", ErrRefreshInvalid
	}

	// 3. 旧 rtid 落墓碑(值=uid,重用时可定位用户)
	sess, err := s.Get(ctx, sid)
	if err != nil {
		return "", "", fmt.Errorf("sessionx: refresh 指向的会话不存在: %w", err)
	}
	if err := s.rdb.Set(ctx, s.key("rt_tomb:", rtid), fmt.Sprintf("%d", sess.UID), s.refreshTTL).Err(); err != nil {
		return "", "", fmt.Errorf("sessionx: 写 refresh 墓碑失败: %w", err)
	}

	// 4. 会话滑动续期 + 换发新 rtid
	_ = s.rdb.Expire(ctx, s.key("sess:", sid), s.sessionTTL).Err()
	newRTID, err = s.IssueRefresh(ctx, sess.UID, sid)
	if err != nil {
		return "", "", err
	}
	return newRTID, sid, nil
}
