package qoder

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
)

var bigLen = big.NewInt(int64(len(pkceAlphabet)))

// cryptoRandInt 返回 [0, n) 的加密随机整数。
func cryptoRandInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// randomUUID 生成 UUIDv4 字符串。
func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}
