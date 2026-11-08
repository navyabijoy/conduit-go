package sdk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// VerifyHMACSHA256 reports whether signature is the hex HMAC-SHA256 of payload under
// secret. An empty secret never verifies, so an unconfigured instance rejects everything.
func VerifyHMACSHA256(payload []byte, signature string, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)
	actualMAC, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(actualMAC, expectedMAC)
}

// VerifyHMACSHA256HexSignature does the same for headers that prefix the digest,
// like GitHub's "sha256=".
func VerifyHMACSHA256HexSignature(payload []byte, signatureHeader string, prefix string, secret []byte) bool {
	return VerifyHMACSHA256(payload, strings.TrimPrefix(signatureHeader, prefix), secret)
}
