package security

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

func TestTOTPStandardVectorsAndReplay(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, tc := range []struct {
		at   int64
		code string
	}{{59, "94287082"}, {1111111109, "07081804"}, {1111111111, "14050471"}, {1234567890, "89005924"}, {2000000000, "69279037"}, {20000000000, "65353130"}} {
		got, err := totpAt(secret, tc.at/30, 8)
		if err != nil || got != tc.code {
			t.Fatalf("vector %d: %s %v", tc.at, got, err)
		}
	}
	now := time.Unix(1234567890, 0)
	code, _ := TOTPCode(secret, now)
	step, err := VerifyTOTP(secret, code, now.Add(30*time.Second), -1)
	if err != nil || step != now.Unix()/30 {
		t.Fatal("clock window")
	}
	if _, err := VerifyTOTP(secret, code, now, step); err == nil {
		t.Fatal("replay accepted")
	}
	if _, err := VerifyTOTP(secret, code, now.Add(61*time.Second), -1); err == nil {
		t.Fatal("expired accepted")
	}
}

func TestTwoFactorEncryptionAndRecovery(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	secret, _ := NewTOTPSecret()
	sealed, err := SealTOTP(key, 10, secret)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := OpenTOTP(key, 10, sealed); err != nil || got != secret {
		t.Fatal("roundtrip")
	}
	for _, tc := range []struct {
		key   []byte
		uid   int64
		value string
	}{{key, 11, sealed}, {[]byte(strings.Repeat("x", 32)), 10, sealed}, {key, 10, sealed + "x"}, {nil, 10, sealed}} {
		if _, err := OpenTOTP(tc.key, tc.uid, tc.value); err == nil {
			t.Fatal("invalid ciphertext/key accepted")
		}
	}
	codes, hashes, err := NewRecoveryCodes()
	if err != nil || len(codes) != 10 {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, code := range codes {
		if seen[code] || hashes[i] != RecoveryDigest(strings.ToLower(code)) || hashes[i] == code {
			t.Fatal("recovery code")
		}
		seen[code] = true
	}
}
