package sessions

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// Signer produces and verifies tamper-proof strings using HMAC-SHA256. It is
// used for cookies that are not session tokens (nickname, OIDC login state).
type Signer struct{ key []byte }

func NewSigner(secret string) *Signer {
	// Derive distinct keys per purpose from the one configured secret.
	return &Signer{key: []byte(secret)}
}

// Sign returns "<payload>.<mac>" for the given purpose and payload.
func (s *Signer) Sign(purpose, payload string) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return p + "." + s.mac(purpose, p)
}

// Verify checks a value produced by Sign and returns the payload.
func (s *Signer) Verify(purpose, signed string) (string, error) {
	p, mac, ok := strings.Cut(signed, ".")
	if !ok {
		return "", errors.New("malformed signed value")
	}
	if !hmac.Equal([]byte(mac), []byte(s.mac(purpose, p))) {
		return "", errors.New("bad signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Signer) mac(purpose, payload string) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
