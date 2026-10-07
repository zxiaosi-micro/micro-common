package jwtauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func genKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥失败: %v", err)
	}
	return key
}

func mustSigner(t *testing.T, kid string) *Signer {
	t.Helper()
	s, err := NewSigner(kid, genKey(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testClaims(sid string) Claims {
	return Claims{
		UID:    10086,
		SID:    sid,
		Client: "ADMIN_WEB",
		Level:  1,
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	signer := mustSigner(t, "key-2026q4")
	verifier, err := NewVerifier(map[string]*rsa.PublicKey{
		"key-2026q4": &signer.activeKey.PublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	token, err := signer.Sign(testClaims("sess-1"), DefaultAccessTTL)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(token)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if claims.UID != 10086 || claims.SID != "sess-1" || claims.Client != "ADMIN_WEB" || claims.Level != 1 {
		t.Fatalf("claims 还原不符: %+v", claims)
	}
	if claims.JTI == "" || len(claims.JTI) != 32 {
		t.Fatalf("jti 应为 32 位 hex: %q", claims.JTI)
	}
	// exp 应为 ~30min 后
	if until := time.Until(claims.ExpiresAt.Time); until < 29*time.Minute || until > 31*time.Minute {
		t.Fatalf("exp 距今 %v, 应约 30min", until)
	}
}

// 验收口径:header 内置 kid,验签支持多公钥集合(轮换期新旧并存)。
func TestKIDRotationBothKeysValid(t *testing.T) {
	oldSigner := mustSigner(t, "key-2026q3")
	newSigner := mustSigner(t, "key-2026q4")

	verifier, err := NewVerifier(map[string]*rsa.PublicKey{
		"key-2026q3": &oldSigner.activeKey.PublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldToken, err := oldSigner.Sign(testClaims("sess-old"), 0)
	if err != nil {
		t.Fatal(err)
	}
	// 轮换:注册新钥,新 token 可验
	if err := verifier.RegisterKey("key-2026q4", &newSigner.activeKey.PublicKey); err != nil {
		t.Fatal(err)
	}
	newToken, err := newSigner.Sign(testClaims("sess-new"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(oldToken); err != nil {
		t.Fatalf("轮换期旧 token 应仍可验: %v", err)
	}
	if _, err := verifier.Verify(newToken); err != nil {
		t.Fatalf("轮换期新 token 应可验: %v", err)
	}
	// 观察期结束:移除旧钥,旧 token 立即失效
	verifier.RevokeKey("key-2026q3")
	if verifier.KeyCount() != 1 {
		t.Fatalf("移除后应只剩 1 把钥: %d", verifier.KeyCount())
	}
	if _, err := verifier.Verify(oldToken); err == nil {
		t.Fatal("旧钥移除后旧 token 应拒绝")
	}
	if _, err := verifier.Verify(newToken); err != nil {
		t.Fatalf("新 token 应不受影响: %v", err)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	signer := mustSigner(t, "key-1")
	other := mustSigner(t, "key-2")
	verifier, err := NewVerifier(map[string]*rsa.PublicKey{"key-1": &signer.activeKey.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	good, _ := signer.Sign(testClaims("s1"), 0)

	// 未知 kid
	foreign, _ := other.Sign(testClaims("s2"), 0)
	if _, err := verifier.Verify(foreign); err == nil {
		t.Fatal("未知 kid 应拒绝")
	}
	// 缺 kid(手工剥掉 header kid 后重签出无 kid token)
	bare := jwt.NewWithClaims(jwt.SigningMethodRS256, testClaims("s3"))
	bareNoKid, err := bare.SignedString(signer.activeKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(bareNoKid); err == nil {
		t.Fatal("缺 kid 应拒绝")
	}
	// 算法混淆:HS256 + 公钥当 HMAC 密钥
	hsToken := jwt.NewWithClaims(jwt.SigningMethodHS256, testClaims("s4"))
	hsStr, err := hsToken.SignedString([]byte("attacker"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(hsStr); err == nil {
		t.Fatal("HS256 混淆攻击应拒绝")
	}
	// 用错公钥验签(签名不匹配)
	wrongVerifier, _ := NewVerifier(map[string]*rsa.PublicKey{"key-1": &other.activeKey.PublicKey})
	if _, err := wrongVerifier.Verify(good); err == nil {
		t.Fatal("签名不匹配应拒绝")
	}
	// 过期 token
	expiredToken, _ := signer.Sign(testClaims("s5"), -time.Minute)
	if _, err := verifier.Verify(expiredToken); err == nil {
		t.Fatal("过期 token 应拒绝")
	}
	// 垃圾串
	if _, err := verifier.Verify("garbage.token.value"); err == nil {
		t.Fatal("垃圾串应拒绝")
	}
}

func TestSignValidation(t *testing.T) {
	signer := mustSigner(t, "key-1")
	if _, err := signer.Sign(Claims{UID: 1}, 0); err == nil {
		t.Fatal("缺 sid 应拒绝签发")
	}
	if _, err := NewSigner("", genKey(t)); err == nil {
		t.Fatal("空 kid 应拒绝")
	}
	if _, err := NewSigner("kid", nil); err == nil {
		t.Fatal("空私钥应拒绝")
	}
}

func TestPEMParseRoundTrip(t *testing.T) {
	key := genKey(t)
	// PKCS#8 私钥 + PKIX 公钥
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	parsed, err := ParsePrivateKeyPEM(privPEM)
	if err != nil {
		t.Fatalf("PKCS#8 私钥解析失败: %v", err)
	}
	if parsed.N == nil || parsed.N.Cmp(key.N) != 0 {
		t.Fatal("私钥内容不符")
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	parsedPub, err := ParsePublicKeyPEM(pubPEM)
	if err != nil || parsedPub.N == nil {
		t.Fatalf("公钥解析失败: %v", err)
	}
	// PKCS#1 兼容
	p1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := ParsePrivateKeyPEM(p1); err != nil {
		t.Fatalf("PKCS#1 兼容失败: %v", err)
	}
	if _, err := ParsePrivateKeyPEM([]byte("not pem")); err == nil {
		t.Fatal("非 PEM 应报错")
	}
	// MustPublicVerifier 接线
	v, err := MustPublicVerifier(map[string][]byte{"key-1": pubPEM})
	if err != nil || v.KeyCount() != 1 {
		t.Fatalf("MustPublicVerifier 失败: %v", err)
	}
}
