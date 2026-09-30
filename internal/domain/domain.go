// Package domain holds small pure helpers shared across the application.
package domain

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"path"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	MaxNicknameLen = 40
	MaxTagLen      = 64
	UploaderPrefix = "uploader/"
	// CalibrationTag marks photos of the clock page (see the images package).
	CalibrationTag = "calibration"
)

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewID returns a random, URL- and filesystem-safe identifier.
func NewID() string {
	var b [15]byte
	mustRand(b[:])
	return idEncoding.EncodeToString(b[:]) // 24 chars
}

// NewToken returns a cryptographically random token (256 bits) for links,
// sessions and CSRF protection.
func NewToken() string {
	var b [32]byte
	mustRand(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func mustRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
}

// CleanNickname trims and validates a user-entered nickname.
func CleanNickname(s string) (string, error) {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return "", UserErr("err.nickname_empty", "please enter a nickname")
	}
	if len([]rune(s)) > MaxNicknameLen {
		return "", UserErr("err.nickname_long", "nickname is too long", map[string]any{"Max": MaxNicknameLen})
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", UserErr("err.nickname_invalid", "nickname contains invalid characters")
		}
	}
	return s, nil
}

// Slug converts arbitrary text to a lowercase [a-z0-9-] slug. Accents are
// folded (é -> e); runs of other characters collapse into a single dash.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFKD.String(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
			dash = false
		case unicode.Is(unicode.Mn, r):
			// combining accent, dropped
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > MaxNicknameLen {
		out = strings.TrimRight(out[:MaxNicknameLen], "-")
	}
	if out == "" {
		out = "anonymous"
	}
	return out
}

// UploaderTag returns the tag name recording who uploaded an image.
func UploaderTag(nickname string) string { return UploaderPrefix + Slug(nickname) }

// NormalizeTag validates and canonicalizes a user-entered tag: trimmed,
// whitespace collapsed and lowercased (tags are case-insensitive, and we
// present them lowercase everywhere).
func NormalizeTag(s string) (string, error) {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return "", UserErr("err.tag_empty", "tag is empty")
	}
	if len([]rune(s)) > MaxTagLen {
		return "", UserErr("err.tag_long", "tag is too long", map[string]any{"Max": MaxTagLen})
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ',' || r == '<' || r == '>' {
			return "", UserErr("err.tag_invalid", "tag contains invalid characters")
		}
	}
	return s, nil
}

// SafeFilename reduces a client-supplied filename to a harmless base name for
// display and downloads. It is never used as a storage key.
func SafeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' || r == ':' || r == '*' || r == '?' || r == '<' || r == '>' || r == '|' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "image"
	}
	if r := []rune(name); len(r) > 200 {
		ext := path.Ext(name)
		if len(ext) > 10 {
			ext = ""
		}
		name = string(r[:200-len([]rune(ext))]) + ext
	}
	return name
}

// UniqueNames makes filenames unique (case-insensitively) by adding an _N
// suffix before the extension: IMG.jpg, IMG_2.jpg, IMG_3.jpg. Uniqueness is
// judged on the name without extension, so that XMP sidecars (stem.xmp) of
// IMG.jpg and IMG.png cannot collide either. The result has the same order
// as names.
func UniqueNames(names []string) []string {
	used := map[string]bool{}
	out := make([]string, len(names))
	for i, n := range names {
		ext := path.Ext(n)
		stem := strings.TrimSuffix(n, ext)
		cand := stem
		for k := 2; used[strings.ToLower(cand)]; k++ {
			cand = stem + "_" + strconv.Itoa(k)
		}
		used[strings.ToLower(cand)] = true
		out[i] = cand + ext
	}
	return out
}

// UserError is a validation error shown to the user. Key is the message ID of
// the catalog entry and Args its template data; the layer that knows the
// request language translates it. Error returns the English text for logs.
type UserError struct {
	Key  string
	Args map[string]any
	Msg  string
}

func (e *UserError) Error() string { return e.Msg }

// UserErr builds a UserError; args is optional.
func UserErr(key, msg string, args ...map[string]any) *UserError {
	e := &UserError{Key: key, Msg: msg}
	if len(args) > 0 {
		e.Args = args[0]
	}
	return e
}
