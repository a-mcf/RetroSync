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
