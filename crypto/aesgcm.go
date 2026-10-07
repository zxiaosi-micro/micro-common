package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// envelopeVersion 密文信封版本号(当前 v1:1B 版本 + 1B kid 长度 + kid + 12B nonce + GCM 密文)。
const envelopeVersion byte = 1

// KeyProvider 数据密钥提供方。dev 用 EnvKeyProvider(环境变量注入,E11);
// 生产接 KMS/Vault 时实现本接口即可,Encryptor 代码零改动。
type KeyProvider interface {
	// GetKey 返回 kid 对应的 32 字节密钥;未知 kid 返回 error(轮换期外不认识即拒绝)。
	GetKey(kid string) ([]byte, error)
	// ActiveKID 当前加密所用的 kid(轮换 = 切换 active + 保留旧钥可解)。
	ActiveKID() string
}

// EnvKeyProvider 基于环境变量的 KeyProvider 起步实现。
type EnvKeyProvider struct {
	active string
	keys   map[string][]byte
}

// NewEnvKeyProvider 构造:activeKID 为当前加密 kid,keys 为 kid→32B 密钥集合。
func NewEnvKeyProvider(activeKID string, keys map[string][]byte) (*EnvKeyProvider, error) {
	if activeKID == "" {
		return nil, errors.New("crypto: activeKID 不允许为空")
	}
	if len(keys) == 0 {
		return nil, errors.New("crypto: 密钥集合不允许为空")
	}
	for kid, key := range keys {
		if len(key) != 32 {
			return nil, fmt.Errorf("crypto: kid %q 密钥长度 %d,应为 AES-256 的 32 字节", kid, len(key))
		}
	}
	if _, ok := keys[activeKID]; !ok {
		return nil, fmt.Errorf("crypto: activeKID %q 不在密钥集合中", activeKID)
	}
	return &EnvKeyProvider{active: activeKID, keys: keys}, nil
}

// EnvKeyProviderFromEnv 从环境变量构造:
//   - MICRO_DATA_KEYS:"kid1=<base64url 32B>,kid2=<base64url 32B>"(轮换期新旧并存);
//   - MICRO_DATA_KEY_KID:当前加密 kid(缺省取集合第一个)。
func EnvKeyProviderFromEnv() (*EnvKeyProvider, error) {
	spec := os.Getenv("MICRO_DATA_KEYS")
	if spec == "" {
		return nil, errors.New("crypto: 环境变量 MICRO_DATA_KEYS 未设置(格式 kid=<base64url>,...)")
	}
	keys, err := ParseKeySpec(spec)
	if err != nil {
		return nil, err
	}
	active := os.Getenv("MICRO_DATA_KEY_KID")
	if active == "" {
		for kid := range keys {
			active = kid
			break
		}
	}
	return NewEnvKeyProvider(active, keys)
}

// ParseKeySpec 解析 "kid1=<base64url>,kid2=<base64url>" 密钥规格。
func ParseKeySpec(spec string) (map[string][]byte, error) {
	keys := make(map[string][]byte)
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kid, raw, ok := strings.Cut(part, "=")
		if !ok || kid == "" || raw == "" {
			return nil, fmt.Errorf("crypto: 密钥规格片段 %q 非法(应为 kid=<base64url>)", part)
		}
		key, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("crypto: kid %q 密钥 base64 解码失败: %w", kid, err)
		}
		keys[kid] = key
	}
	return keys, nil
}

func (p *EnvKeyProvider) GetKey(kid string) ([]byte, error) {
	key, ok := p.keys[kid]
	if !ok {
		return nil, fmt.Errorf("crypto: 未知密钥 kid %q", kid)
	}
	return key, nil
}

func (p *EnvKeyProvider) ActiveKID() string { return p.active }

// Encryptor AES-256-GCM 信封加密器。
type Encryptor struct {
	provider KeyProvider
}

// NewEncryptor 构造加密器。
func NewEncryptor(provider KeyProvider) (*Encryptor, error) {
	if provider == nil {
		return nil, errors.New("crypto: KeyProvider 不允许为空")
	}
	return &Encryptor{provider: provider}, nil
}

// Encrypt 加密并输出 base64 信封:头部携带 kid,轮换后旧密文凭旧 kid 仍可解。
// kid 同时作为 GCM AAD 绑定,防止密文被改标到其他 kid。
func (e *Encryptor) Encrypt(plaintext []byte) (string, error) {
	kid := e.provider.ActiveKID()
	key, err := e.provider.GetKey(kid)
	if err != nil {
		return "", err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: 生成 nonce 失败: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, []byte(kid))

	buf := make([]byte, 0, 2+len(kid)+len(nonce)+len(sealed))
	buf = append(buf, envelopeVersion, byte(len(kid)))
	buf = append(buf, kid...)
	buf = append(buf, nonce...)
	buf = append(buf, sealed...)
	return base64.StdEncoding.EncodeToString(buf), nil
}

// Decrypt 解析信封头 kid → 取对应密钥解密。
// 轮换后:新密文用新 kid,旧密文凭保留的旧 kid 解密——往返+轮换可解由单测固化。
func (e *Encryptor) Decrypt(envelope string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(envelope)
	if err != nil {
		return nil, fmt.Errorf("crypto: 密文 base64 解码失败: %w", err)
	}
	if len(raw) < 2 || raw[0] != envelopeVersion {
		return nil, errors.New("crypto: 密文信封版本不支持")
	}
	kidLen := int(raw[1])
	if len(raw) < 2+kidLen {
		return nil, errors.New("crypto: 密文信封头不完整")
	}
	kid := string(raw[2 : 2+kidLen])
	key, err := e.provider.GetKey(kid)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	rest := raw[2+kidLen:]
	if len(rest) < aead.NonceSize() {
		return nil, errors.New("crypto: 密文 nonce 缺失")
	}
	nonce, sealed := rest[:aead.NonceSize()], rest[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, sealed, []byte(kid))
	if err != nil {
		return nil, fmt.Errorf("crypto: 密文解密失败(kid=%s): %w", kid, err)
	}
	return plaintext, nil
}

// PeekKID 只解析密文 kid 不解密(排障/轮换盘点用)。
func PeekKID(envelope string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(envelope)
	if err != nil {
		return "", fmt.Errorf("crypto: 密文 base64 解码失败: %w", err)
	}
	if len(raw) < 2 || raw[0] != envelopeVersion {
		return "", errors.New("crypto: 密文信封版本不支持")
	}
	kidLen := int(raw[1])
	if len(raw) < 2+kidLen {
		return "", errors.New("crypto: 密文信封头不完整")
	}
	return string(raw[2 : 2+kidLen]), nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: 构造 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: 构造 GCM 失败: %w", err)
	}
	return aead, nil
}

// GenerateKey 生成 32 字节随机数据密钥(返回 base64url,可直接写入环境变量)。
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("crypto: 生成密钥失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}
