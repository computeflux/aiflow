package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/computeflux/auth/config"
	"github.com/computeflux/auth/model"
)

// WalletLogin 外部钱包（MetaMask / WalletConnect）登录：
// EIP-191 personal_sign 签名验证身份（迁移自 tee-node 的 loginWithWallet 流程）。
// 前端（web3auth SDK）流程：requestWalletNonce → 钱包签名 (message+nonce) → loginWithWallet。
type WalletLogin struct {
	users   *UserSvc
	session *Session
	cfg     *config.Config

	mu    sync.Mutex
	nonce map[string]walletNonceRec // key = 小写地址
}

type walletNonceRec struct {
	nonce     string
	expiresAt int64
	used      bool
}

func NewWalletLogin(users *UserSvc, session *Session, cfg *config.Config) *WalletLogin {
	return &WalletLogin{
		users:   users,
		session: session,
		cfg:     cfg,
		nonce:   map[string]walletNonceRec{},
	}
}

// RequestNonce 为地址生成一次性 nonce（300s 有效，防重放）。
func (w *WalletLogin) RequestNonce(address string) (string, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" {
		return "", model.NewErr(400, "address required")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(b)
	w.mu.Lock()
	w.nonce[address] = walletNonceRec{nonce: nonce, expiresAt: time.Now().Unix() + 300, used: false}
	w.mu.Unlock()
	return nonce, nil
}

// LoginWithWallet 验证 EIP-191 签名（签名内容 = message + nonce）→ upsert 用户 → 签发 token 对。
func (w *WalletLogin) LoginWithWallet(address, signature, message, nonce, ip string) (*model.LoginResult, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" || nonce == "" {
		return nil, model.NewErr(400, "address and nonce required")
	}
	w.mu.Lock()
	rec, ok := w.nonce[address]
	w.mu.Unlock()
	if !ok || rec.nonce != nonce {
		return nil, model.NewErr(401, "invalid or expired nonce")
	}
	if rec.used {
		return nil, model.NewErr(401, "nonce already used")
	}
	if time.Now().Unix() > rec.expiresAt {
		return nil, model.NewErr(401, "nonce expired")
	}
	// 验证 EIP-191 签名（签名内容 = message + nonce）
	if !verifyEIP191Signature(address, signature, message+nonce) {
		return nil, model.NewErr(401, "invalid signature")
	}
	// 标记已用（防重放）
	w.mu.Lock()
	rec.used = true
	w.nonce[address] = rec
	w.mu.Unlock()

	// upsert 用户：身份 = 钱包地址（Provider=wallet, Subject=address）
	user, err := w.users.Upsert(&Profile{
		Provider:   "wallet",
		Subject:    address,
		WalletAddr: address,
	})
	if err != nil {
		return nil, err
	}
	if user.Status != 1 {
		return nil, model.NewErr(403, "user disabled")
	}
	return w.session.Issue(user, "wallet", ip)
}

// verifyEIP191Signature 验证 EIP-191 personal_sign 签名（迁移自 tee-node graph/auth_helpers.go）。
func verifyEIP191Signature(address, signatureHex, message string) bool {
	sigBytes, err := hex.DecodeString(strings.TrimPrefix(signatureHex, "0x"))
	if err != nil || len(sigBytes) != 65 {
		return false
	}
	// EIP-191: "\x19Ethereum Signed Message:\n" + len(message) + message
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	hash := ethcrypto.Keccak256Hash([]byte(msg))
	// V 从 27/28 转 0/1 用于 recovery
	if sigBytes[64] >= 27 {
		sigBytes[64] -= 27
	}
	pubKey, err := ethcrypto.SigToPub(hash.Bytes(), sigBytes)
	if err != nil {
		return false
	}
	recoveredAddr := ethcrypto.PubkeyToAddress(*pubKey).Hex()
	return strings.EqualFold(recoveredAddr, address)
}
