// Package auth provides password hashing and verification for RetroSync web
// users, using argon2id (the password-hashing winner, memory-hard and
// resistant to GPU/ASIC cracking). Hashes are stored in the standard PHC
// encoded string form so the parameters and per-hash salt travel with the
// hash; verification is constant-time.
//
// This package deliberately knows nothing about the Store or HTTP: it is a
// pure crypto helper so it can be unit-tested in isolation and reused by both
// the web login path and the `user set` CLI bootstrap.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Tuning parameters for argon2id. These follow the OWASP-recommended baseline
// (memory ~64 MiB, a few iterations, parallelism 1) — a sane default for a
// low-traffic household tool where logins are rare and a per-hash cost in the
// tens of milliseconds is invisible to the user but expensive to an attacker.
//
// The parameters are encoded into every hash, so they may be raised later
// without breaking verification of older hashes.
const (
	argonTime    = 3         // iterations (time cost)
	argonMemory  = 64 * 1024 // KiB => 64 MiB
	argonThreads = 1         // lanes / parallelism
	argonKeyLen  = 32        // derived key length (bytes)
	saltLen      = 16        // per-hash random salt length (bytes)
)

// MinPasswordLen is the shortest password RetroSync accepts anywhere a password
// is SET: the `user set` CLI bootstrap, the admin /users create + reset forms,
// and the self-service change. Length is the only rule — no composition
// requirements, which push people toward "P@ssw0rd!" — and 8 is the NIST 800-63B
// floor. Login does NOT apply it: an existing shorter password must still be
// able to sign in (and be changed).
const MinPasswordLen = 8

// ErrPasswordTooShort is returned by ValidatePassword for a password below
// MinPasswordLen (including an empty one). Its message is safe to show a human.
var ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)

// ValidatePassword reports whether a proposed password is acceptable to set. It
// is the single rule shared by every path that sets a password, so the CLI and
// the web UI can never drift apart. Length is measured in runes, so a short
// non-ASCII passphrase isn't over-credited by its byte count.
func ValidatePassword(password string) error {
	if len([]rune(password)) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	return nil
}

// ErrInvalidHash is returned by Verify when the stored encoded hash is
// malformed (wrong format, bad version, unparseable params, or corrupt
// base64). It is distinct from "password did not match" so callers can tell a
// data-integrity problem from a wrong password.
var ErrInvalidHash = errors.New("auth: invalid encoded hash")

// Hash derives an argon2id hash of password and returns it in the standard PHC
// encoded form:
//
//	$argon2id$v=19$m=65536,t=3,p=1$<b64salt>$<b64hash>
//
// A fresh 128-bit random salt is generated per call, so hashing the same
// password twice yields different strings.
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	b64 := base64.RawStdEncoding.EncodeToString
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64(salt), b64(key),
	)
	return encoded, nil
}

// Verify reports whether password matches the PHC-encoded argon2id hash. It
// returns (false, ErrInvalidHash) if encodedHash is malformed, and
// (false, nil) for a well-formed hash that simply does not match. The
// comparison is constant-time.
func Verify(encodedHash, password string) (bool, error) {
	p, salt, key, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	other := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(key)))
	if subtle.ConstantTimeCompare(key, other) == 1 {
		return true, nil
	}
	return false, nil
}

// params are the argon2 cost parameters decoded out of an encoded hash.
type params struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeHash parses a PHC argon2id string into its params, salt, and derived
// key. Any structural problem yields ErrInvalidHash (wrapped for context); we
// never return the raw parse error to callers to avoid leaking hash internals
// into logs.
func decodeHash(encoded string) (params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "" / argon2id / v=.. / m=..,t=..,p=.. / salt / hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params{}, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params{}, nil, nil, fmt.Errorf("%w: version", ErrInvalidHash)
	}
	if version != argon2.Version {
		return params{}, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidHash, version)
	}

	var p params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return params{}, nil, nil, fmt.Errorf("%w: params", ErrInvalidHash)
	}
	// Bounds: argon2.IDKey PANICS on time < 1 or threads < 1, and requires
	// memory >= 8*threads (KiB per lane). A hash encoding such params is
	// malformed data, not a wrong password — per the package contract it must
	// yield ErrInvalidHash, never a panic.
	if p.time < 1 || p.threads < 1 || p.memory < 8*uint32(p.threads) {
		return params{}, nil, nil, fmt.Errorf("%w: params out of range", ErrInvalidHash)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params{}, nil, nil, fmt.Errorf("%w: salt", ErrInvalidHash)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params{}, nil, nil, fmt.Errorf("%w: key", ErrInvalidHash)
	}
	if len(key) == 0 {
		return params{}, nil, nil, fmt.Errorf("%w: empty key", ErrInvalidHash)
	}
	return p, salt, key, nil
}
