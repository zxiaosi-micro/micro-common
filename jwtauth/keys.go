package jwtauth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// PEM 解析辅助:与 keygen(S2-04)产出的密钥文件格式对接。

// ParsePrivateKeyPEM 解析 RSA 私钥 PEM(PKCS#8 优先,兼容 PKCS#1)。
func ParsePrivateKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("jwtauth: PEM 解析失败(私钥文件为空或格式错误)")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("jwtauth: 私钥不是 RSA 类型")
	}
	if rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rsaKey, nil
	}
	return nil, errors.New("jwtauth: 私钥既非 PKCS#8 也非 PKCS#1")
}

// ParsePublicKeyPEM 解析 RSA 公钥 PEM(PKIX 优先,兼容 PKCS#1)。
func ParsePublicKeyPEM(data []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("jwtauth: PEM 解析失败(公钥文件为空或格式错误)")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("jwtauth: 公钥不是 RSA 类型")
	}
	if rsaKey, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return rsaKey, nil
	}
	return nil, errors.New("jwtauth: 公钥既非 PKIX 也非 PKCS#1")
}

// MustPublicVerifier 从 PEM 公钥集合构造 Verifier(kid→PEM 内容),配置接线便捷入口。
func MustPublicVerifier(pems map[string][]byte) (*Verifier, error) {
	keys := make(map[string]*rsa.PublicKey, len(pems))
	for kid, pemBytes := range pems {
		pub, err := ParsePublicKeyPEM(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("jwtauth: 解析 kid %q 公钥失败: %w", kid, err)
		}
		keys[kid] = pub
	}
	return NewVerifier(keys)
}
