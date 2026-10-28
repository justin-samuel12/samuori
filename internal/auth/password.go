// Package auth implements local authentication: passwords, sessions, API tokens and first-run bootstrap.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params is a package variable so tests can lower memory cost; production uses the defaults.
var Argon2Params = struct {
	Time, Memory uint32
	Threads      uint8
}{Time: 3, Memory: 64 * 1024, Threads: 2}

func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := Argon2Params
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, t uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &t, &threads); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var ErrWeakPassword = errors.New("password must be 12–1024 characters and must not contain the username")

func CheckPasswordPolicy(username, password string) error {
	if len(password) < 12 || len(password) > 1024 {
		return ErrWeakPassword
	}
	if username != "" && strings.Contains(strings.ToLower(password), strings.ToLower(username)) {
		return ErrWeakPassword
	}
	return nil
}

// newSecret returns a random token and the SHA-256 hash that is stored instead of it.
func newSecret() (token, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashSecret(token)
}

func hashSecret(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
