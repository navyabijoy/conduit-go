package sdk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifyWebhookSignature(t *testing.T) {
	secret := []byte("my_super_secret_webhook_key")
	payload := []byte(`{"event":"test","status":"ok"}`)
	sig := hmacSignHex(payload, secret)

	t.Run("valid signature", func(t *testing.T) {
		if !VerifyHMACSHA256(payload, sig, secret) {
			t.Errorf("HMAC signature verification failed for signature %s", sig)
		}
	})

	t.Run("valid prefixed signature", func(t *testing.T) {
		if !VerifyHMACSHA256HexSignature(payload, "sha256="+sig, "sha256=", secret) {
			t.Errorf("Prefixed HMAC signature verification failed")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		if VerifyHMACSHA256(payload, sig, []byte("wrong_secret")) {
			t.Errorf("Expected signature verification to fail with incorrect secret, but it succeeded")
		}
	})

	t.Run("empty secret never verifies", func(t *testing.T) {
		if VerifyHMACSHA256(payload, hmacSignHex(payload, nil), nil) {
			t.Errorf("Expected an empty secret to be rejected")
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		corruptedPayload := []byte(`{"event":"test","status":"corrupted"}`)
		if VerifyHMACSHA256(corruptedPayload, sig, secret) {
			t.Errorf("Expected signature verification to fail with corrupted payload, but it succeeded")
		}
	})
}

func hmacSignHex(payload []byte, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
