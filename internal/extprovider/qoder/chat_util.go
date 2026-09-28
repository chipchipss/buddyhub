package qoder

import (
	"crypto/rand"
	"encoding/hex"
)

// crandReadQ crypto/rand.Read 别名。
func crandReadQ(b []byte) (int, error) { return rand.Read(b) }

// hexEncodeQ hex 编码。
func hexEncodeQ(b [16]byte) string { return hex.EncodeToString(b[:]) }
