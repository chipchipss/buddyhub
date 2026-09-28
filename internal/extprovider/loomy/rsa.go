// Package loomy: rsa.go — RSA/PKCS#1 v1.5 加密（对齐 loomy2api/crypto.py）。
//
// getPuKey 下发的是 DER SubjectPublicKeyInfo base64。Go 标准库直接解析
// PKIX 公钥（x509.ParsePKIXPublicKey），兼容 PKCS#1 RSAPublicKey 形态。
package loomy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
)

// RSAEncryptPKCS1v15FromB64DER 用 base64(DER SPKI) 公钥加密明文 → base64 密文。
func RSAEncryptPKCS1v15FromB64DER(pubkeyB64, plaintext string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(padB64(pubkeyB64))
	if err != nil {
		return "", fmt.Errorf("公钥 base64 解码失败: %w", err)
	}
	pubAny, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		// 兼容 PKCS#1 RSAPublicKey 形态
		rsaPub, err1 := x509.ParsePKCS1PublicKey(der)
		if err1 != nil {
			return "", fmt.Errorf("公钥解析失败（PKIX 与 PKCS#1 均不识别）: %w", err)
		}
		pubAny = rsaPub
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("公钥类型不是 RSA")
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("RSA 加密失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// padB64 补齐 base64 缺失的 =（服务端下发的公钥常无 padding）。
func padB64(s string) string {
	if m := len(s) % 4; m != 0 {
		for i := 0; i < 4-m; i++ {
			s += "="
		}
	}
	return s
}
