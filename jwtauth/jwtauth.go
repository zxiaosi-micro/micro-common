// Package jwtauth 实现 RS256 access token 的签发与校验。
//
// 设计(02 §9.5 / ADR-11):
//   - claims 极简:uid/sid/client/level/jti——权限不进 JWT(塞进去 30 分钟内无法回收);
//   - header 内置 kid,验签方持多公钥集合按 kid 选钥——密钥轮换期新旧并存、零停机;
//   - access TTL 默认 30min;会话权威状态在 sessionx(Redis DB4)。
package jwtauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultAccessTTL access token 默认有效期(30min)。
const DefaultAccessTTL = 30 * time.Minute

// signingAlg 签名算法:固定 RS256,不接受其他算法(防算法混淆攻击)。
var signingAlg = jwt.SigningMethodRS256.Alg()

// Claims 极简业务 claims(权限不进 JWT)。
type Claims struct {
	// UID 用户 ID(雪花 int64)。
	UID int64 `json:"uid"`
	// SID 会话 ID,与 sessionx sess:{sid} 对应,服务端可即时失效。
	SID string `json:"sid"`
	// Client 登录端(ctxkit.Client* 值域),用于 client 匹配校验。
	Client string `json:"client"`
	// Level 认证级别:1 普通;2 step-up 二级认证后签发。
	Level int `json:"level,omitempty"`
	// JTI token 唯一标识(吊销审计用)。
	JTI string `json:"jti"`
	jwt.RegisteredClaims
}

var (
	// ErrKidMissing token header 缺少 kid。
	ErrKidMissing = errors.New("jwtauth: token 缺少 kid")
	// ErrKidUnknown kid 不在验签公钥集合(轮换期外或伪造)。
	ErrKidUnknown = errors.New("jwtauth: kid 不在公钥集合")
	// ErrAlgMismatch 签名算法非 RS256。
	ErrAlgMismatch = errors.New("jwtauth: 签名算法必须为 RS256")
)

// —— 签发 ——

// Signer RS256 签发器(identity 服务持有私钥)。
type Signer struct {
	activeKID string
	activeKey *rsa.PrivateKey
}

// NewSigner 构造签发器(kid 与私钥由 keygen 产出,S2-04)。
func NewSigner(kid string, key *rsa.PrivateKey) (*Signer, error) {
	if kid == "" {
		return nil, errors.New("jwtauth: kid 不允许为空")
	}
	if key == nil {
		return nil, errors.New("jwtauth: 私钥不允许为空")
	}
	return &Signer{activeKID: kid, activeKey: key}, nil
}

// KID 返回当前签名 kid。
func (s *Signer) KID() string { return s.activeKID }

// Sign 签发 access token:header 带 kid,注册声明带 exp/iat/nbf。
// ttl 为 0 时取 DefaultAccessTTL;负数表示立即过期(测试/作废场景)。
func (s *Signer) Sign(claims Claims, ttl time.Duration) (string, error) {
	if claims.SID == "" {
		return "", errors.New("jwtauth: claims.sid 不允许为空(会话权威依赖 sid)")
	}
	if claims.JTI == "" {
		jti, err := newJTI()
		if err != nil {
			return "", err
		}
		claims.JTI = jti
	}
	if ttl == 0 {
		ttl = DefaultAccessTTL
	}
	now := time.Now()
	claims.ID = claims.JTI
	claims.IssuedAt = jwt.NewNumericDate(now)
	claims.NotBefore = jwt.NewNumericDate(now)
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.activeKID
	signed, err := token.SignedString(s.activeKey)
	if err != nil {
		return "", fmt.Errorf("jwtauth: 签名失败: %w", err)
	}
	return signed, nil
}

// newJTI 128bit 随机 hex。
func newJTI() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("jwtauth: 生成 jti 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// —— 校验 ——

// Verifier 验签器:持多公钥集合(kid→公钥),轮换期新旧并存。
type Verifier struct {
	keys map[string]*rsa.PublicKey
}

// NewVerifier 构造验签器(keys 不允许为空,至少一把公钥)。
func NewVerifier(keys map[string]*rsa.PublicKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("jwtauth: 公钥集合不允许为空")
	}
	for kid, k := range keys {
		if kid == "" {
			return nil, errors.New("jwtauth: 公钥 kid 不允许为空")
		}
		if k == nil {
			return nil, fmt.Errorf("jwtauth: kid %q 公钥为空", kid)
		}
	}
	return &Verifier{keys: keys}, nil
}

// RegisterKey 轮换期注册新公钥(新钥入集合 → 发新 token → 观察期后 RevokeKey 移旧钥)。
func (v *Verifier) RegisterKey(kid string, pub *rsa.PublicKey) error {
	if kid == "" || pub == nil {
		return errors.New("jwtauth: kid/公钥不允许为空")
	}
	v.keys[kid] = pub
	return nil
}

// RevokeKey 观察期结束移除旧公钥;移除后旧 kid token 立即不可验。
func (v *Verifier) RevokeKey(kid string) {
	delete(v.keys, kid)
}

// KeyCount 返回当前公钥数量(轮换演练观察用)。
func (v *Verifier) KeyCount() int { return len(v.keys) }

// Verify 验签并还原 claims:按 header kid 从多公钥集合选钥,
// 强制 RS256,校验 exp/nbf。
func (v *Verifier) Verify(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (any, error) {
		// 算法白名单:拒绝 none/HS256 等算法混淆
		if t.Method.Alg() != signingAlg {
			return nil, ErrAlgMismatch
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, ErrKidMissing
		}
		pub, ok := v.keys[kid]
		if !ok {
			return nil, ErrKidUnknown
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{signingAlg}))
	if err != nil {
		return nil, fmt.Errorf("jwtauth: 验签失败: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, errors.New("jwtauth: token 无效")
	}
	return claims, nil
}
