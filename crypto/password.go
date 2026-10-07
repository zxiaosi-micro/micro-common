// Package crypto 提供平台安全基线:
//   - argon2id 密码哈希(OWASP 参数,PHC 串格式);
//   - AES-256-GCM 信封加密 + KeyProvider(kid 密文头,轮换期旧密文可解,
//     EnvKeyProvider 起步,KMS 补位即插);
//   - Mask* 统一掩码函数与 logx.Sensitive 日志脱敏适配器(02 §11/§13)。
//
// 纪律:密钥一律环境变量注入(E11),仓库零真实密钥。
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP argon2id 推荐参数(v1 定稿:64MB / 1 iter / 4 threads)。
const (
	argon2Memory  uint32 = 64 * 1024 // KiB,即 64MB
	argon2Time    uint32 = 1
	argon2Threads uint8  = 4
	argon2KeyLen  uint32 = 32
	argon2SaltLen        = 16
)

const (
	phcAlg      = "argon2id"
	phcVersion  = 19
	phcSegCount = 6 // $argon2id $v=19 $m=..,t=..,p=.. $salt $hash → split 后 6 段(首段为空)
)

var errInvalidPHC = errors.New("crypto: 非法 argon2id PHC 串")

// HashPassword 生成 argon2id PHC 串(含随机盐与算法参数),直接可入库
// password_hash VARCHAR(255)。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: 生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcAlg, phcVersion, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword 校验明文密码与 PHC 串是否匹配。
// 兼容参数演进:盐/参数/长度全部从 PHC 串解析,旧参数串仍可校验。
func VerifyPassword(password, phc string) (bool, error) {
	alg, _, m, t, p, salt, want, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if alg != phcAlg {
		return false, fmt.Errorf("crypto: 不支持的算法 %q", alg)
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return true, nil
	}
	return false, nil
}

func parsePHC(phc string) (alg string, version int, m uint32, t uint32, p int, salt, key []byte, err error) {
	parts := strings.Split(phc, "$")
	if len(parts) != phcSegCount || parts[0] != "" {
		return "", 0, 0, 0, 0, nil, nil, errInvalidPHC
	}
	alg = parts[1]
	if !strings.HasPrefix(parts[2], "v=") {
		return "", 0, 0, 0, 0, nil, nil, errInvalidPHC
	}
	if version, err = strconv.Atoi(strings.TrimPrefix(parts[2], "v=")); err != nil {
		return "", 0, 0, 0, 0, nil, nil, errInvalidPHC
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 ||
		!strings.HasPrefix(params[0], "m=") ||
		!strings.HasPrefix(params[1], "t=") ||
		!strings.HasPrefix(params[2], "p=") {
		return "", 0, 0, 0, 0, nil, nil, errInvalidPHC
	}
	mv, err1 := strconv.ParseUint(strings.TrimPrefix(params[0], "m="), 10, 32)
	tv, err2 := strconv.ParseUint(strings.TrimPrefix(params[1], "t="), 10, 32)
	pv, err3 := strconv.ParseUint(strings.TrimPrefix(params[2], "p="), 10, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return "", 0, 0, 0, 0, nil, nil, errInvalidPHC
	}
	m, t, p = uint32(mv), uint32(tv), int(pv)
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return "", 0, 0, 0, 0, nil, nil, errInvalidPHC
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return "", 0, 0, 0, 0, nil, nil, errInvalidPHC
	}
	return alg, version, m, t, p, salt, key, nil
}
