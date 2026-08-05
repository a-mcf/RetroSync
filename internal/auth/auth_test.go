package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, err := Verify(h, pw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("Verify: want match for correct password")
	}
}

func TestVerifyWrongPassword(t *testing.T) {
	h, err := Hash("the-right-one")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, err := Verify(h, "the-wrong-one")
	if err != nil {
		t.Fatalf("Verify returned an error for a wrong (but valid) password: %v", err)
	}
	if ok {
		t.Fatal("Verify: want no match for wrong password")
	}
}

func TestVerifyTamperedHashErrors(t *testing.T) {
	good, err := Hash("pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	cases := map[string]string{
		"empty":           "",
		"garbage":         "not-a-hash",
		"wrong-algo":      "$argon2i$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA",
		"bad-version":     "$argon2id$v=99$m=65536,t=3,p=1$c2FsdA$aGFzaA",
		"bad-params":      "$argon2id$v=19$m=nope,t=3,p=1$c2FsdA$aGFzaA",
		"bad-base64-salt": "$argon2id$v=19$m=65536,t=3,p=1$!!!$aGFzaA",
		"truncated":       strings.TrimSuffix(good, "$"+strings.Split(good, "$")[5]),
		// Out-of-range cost params: argon2.IDKey panics on t=0 or p=0 and
		// requires m >= 8*p. These must map to ErrInvalidHash, never a panic.
		"zero-time":     "$argon2id$v=19$m=65536,t=0,p=1$c2FsdA$aGFzaA",
		"zero-threads":  "$argon2id$v=19$m=65536,t=3,p=0$c2FsdA$aGFzaA",
		"absurd-memory": "$argon2id$v=19$m=4,t=3,p=1$c2FsdA$aGFzaA",
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := Verify(h, "pw")
			if ok {
				t.Fatal("Verify: tampered hash must not report a match")
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("Verify(%q): want ErrInvalidHash, got %v", h, err)
			}
		})
	}
}

// TestVerifyZeroTimeParamNoPanic pins the panic-class bug: a well-formed PHC
// string whose t (time) param is 0 used to reach argon2.IDKey, which panics on
// time < 1. It must instead return (false, ErrInvalidHash).
func TestVerifyZeroTimeParamNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Verify panicked on a t=0 hash: %v", r)
		}
	}()
	ok, err := Verify("$argon2id$v=19$m=65536,t=0,p=1$c2FsdA$aGFzaA", "pw")
	if ok {
		t.Fatal("Verify: t=0 hash must not report a match")
	}
	if !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("Verify: want ErrInvalidHash, got %v", err)
	}
}

func TestHashSaltedDiffers(t *testing.T) {
	const pw = "same-password"
	a, err := Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := Hash(pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
	// Both must still verify.
	for _, h := range []string{a, b} {
		ok, err := Verify(h, pw)
		if err != nil || !ok {
			t.Fatalf("Verify(%q) = %v, %v; want true, nil", h, ok, err)
		}
	}
}

// TestValidatePassword pins the one shared set-a-password rule (used by the
// `user set` CLI and every web password form): at least MinPasswordLen RUNES,
// no composition requirements.
func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"empty", "", true},
		{"one-short", strings.Repeat("a", MinPasswordLen-1), true},
		{"exactly-min", strings.Repeat("a", MinPasswordLen), false},
		{"long-passphrase", "correct horse battery staple", false},
		{"multibyte-counts-runes-not-bytes", strings.Repeat("é", MinPasswordLen-1), true},
		{"multibyte-long-enough", strings.Repeat("é", MinPasswordLen), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			if tc.wantErr {
				if !errors.Is(err, ErrPasswordTooShort) {
					t.Fatalf("ValidatePassword(%q) = %v, want ErrPasswordTooShort", tc.password, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidatePassword(%q) = %v, want nil", tc.password, err)
			}
		})
	}
}
