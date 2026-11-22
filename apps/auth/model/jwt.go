package model

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims access token 的 claims（RS256, 15min）。
// roles 是快照：角色变更后旧 token 最长 15min 才失效；即时生效走 /authorize。
type Claims struct {
	Roles  []string `json:"roles,omitempty"`
	Email  string   `json:"email,omitempty"`
	Wallet string   `json:"wallet,omitempty"`
	jwt.RegisteredClaims
}

// KeyPair RS256 密钥对（私钥仅 auth 持有，不落库）。
type KeyPair struct {
	PrivateKey *rsa.PrivateKey
	PublicKey  *rsa.PublicKey
	Kid        string
}

// LoadOrCreateKeyPair 从 PEM 文件加载 RSA 私钥；不存在时生成 2048 位密钥对并持久化（0600）。
// kid 缺省由公钥 DER 指纹（SHA256 前 8 hex）推导，支持多 kid 轮换。
func LoadOrCreateKeyPair(path, kidOverride string) (*KeyPair, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		kp, perr := parsePrivateKeyPEM(data)
		if perr != nil {
			return nil, false, fmt.Errorf("parse %s: %w", path, perr)
		}
		kp.Kid = kidOverrideOrFingerprint(kidOverride, kp.PublicKey)
		return kp, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	key, gerr := rsa.GenerateKey(rand.Reader, 2048)
	if gerr != nil {
		return nil, false, fmt.Errorf("generate rsa key: %w", gerr)
	}
	der, derr := x509.MarshalPKCS8PrivateKey(key)
	if derr != nil {
		return nil, false, derr
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if werr := os.WriteFile(path, pemBytes, 0o600); werr != nil {
		return nil, false, fmt.Errorf("write %s: %w", path, werr)
	}
	kp := &KeyPair{PrivateKey: key, PublicKey: &key.PublicKey}
	kp.Kid = kidOverrideOrFingerprint(kidOverride, kp.PublicKey)
	return kp, true, nil
}

func parsePrivateKeyPEM(data []byte) (*KeyPair, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		key = rk
	default:
		return nil, fmt.Errorf("unsupported PEM type %q", block.Type)
	}
	return &KeyPair{PrivateKey: key, PublicKey: &key.PublicKey}, nil
}

func kidOverrideOrFingerprint(override string, pub *rsa.PublicKey) string {
	if override != "" {
		return override
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "auth-1"
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:4])
}

// Signer 签发/验证 access token。
type Signer struct {
	KeyPair   *KeyPair
	Issuer    string
	Audience  string
	AccessTTL time.Duration
}

// Issue 签发 access token，返回 (token, jti, 过期时间)。
func (s *Signer) Issue(userID string, roles []string, email, wallet string) (string, string, time.Time, error) {
	now := time.Now()
	jti := randomHex(16)
	claims := &Claims{
		Roles:  roles,
		Email:  email,
		Wallet: wallet,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.Issuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{s.Audience},
			ExpiresAt: jwt.NewNumericDate(now.Add(s.AccessTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.KeyPair.Kid
	tok, err := token.SignedString(s.KeyPair.PrivateKey)
	return tok, jti, now.Add(s.AccessTTL), err
}

// Verify 验证 access token（RS256 签名 + iss/aud/exp）。
func (s *Signer) Verify(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.KeyPair.PublicKey, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(s.Issuer),
		jwt.WithAudience(s.Audience),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// JWKS 导出公钥集（kid 索引，支持轮换：多 kid 并存时逐个导出）。
func JWKS(keys []*KeyPair) map[string]any {
	keysOut := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		keysOut = append(keysOut, map[string]any{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": k.Kid,
			"n":   base64.RawURLEncoding.EncodeToString(k.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(intToBytes(k.PublicKey.E)),
		})
	}
	return map[string]any{"keys": keysOut}
}

func intToBytes(v int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	// 去掉前导零（JWKS 的 e 为最小大端表示）
	i := 0
	for i < len(b)-1 && b[i] == 0 {
		i++
	}
	return b[i:]
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
