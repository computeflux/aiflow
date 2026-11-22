package service

import (
	"fmt"
	"testing"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// TestVerifyEIP191Signature 用真实密钥签名 → 验证通过；篡改地址/签名 → 拒绝。
func TestVerifyEIP191Signature(t *testing.T) {
	priv, err := ethcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := ethcrypto.PubkeyToAddress(priv.PublicKey).Hex()
	message := "Sign in to ComputeFlux"

	// EIP-191 personal_sign
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	hash := ethcrypto.Keccak256Hash([]byte(msg))
	sig, err := ethcrypto.Sign(hash.Bytes(), priv)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyEIP191Signature(addr, "0x"+fmt.Sprintf("%x", sig), message) {
		t.Fatal("expected valid signature to pass")
	}
	// 篡改地址 → 拒绝
	if verifyEIP191Signature("0x0000000000000000000000000000000000000001", "0x"+fmt.Sprintf("%x", sig), message) {
		t.Fatal("expected wrong address to fail")
	}
	// 篡改消息 → 拒绝
	if verifyEIP191Signature(addr, "0x"+fmt.Sprintf("%x", sig), message+"x") {
		t.Fatal("expected wrong message to fail")
	}
	// 坏签名 → 拒绝
	if verifyEIP191Signature(addr, "0xdeadbeef", message) {
		t.Fatal("expected bad signature to fail")
	}
}
