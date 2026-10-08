// Package auth implements the portal's bearer-token authentication.
//
// The token is re-read from PORTAL_TOKEN_FILE on every request so that a
// rotated projected secret takes effect without a restart. Missing, empty, or
// weak token files deny every API request.
package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// MinTokenLength is the minimum accepted length of the portal token.
const MinTokenLength = 32

// maxTokenLength bounds how much of the token file is treated as a token.
const maxTokenLength = 4096

var (
	// ErrTokenUnavailable means the token file is missing, unreadable, or weak.
	ErrTokenUnavailable = errors.New("portal token unavailable")
	// ErrUnauthorized means the request carried a missing or wrong token.
	ErrUnauthorized = errors.New("invalid bearer token")
)

// LoadToken reads the portal token from path and verifies its strength.
func LoadToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: token file path is empty", ErrTokenUnavailable)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: cannot read token file: %v", ErrTokenUnavailable, err)
	}
	token := strings.TrimSpace(string(raw))
	if err := CheckStrength(token); err != nil {
		return "", fmt.Errorf("%w: %v", ErrTokenUnavailable, err)
	}
	return token, nil
}

// CheckStrength reports whether token is long enough and free of weak
// patterns such as whitespace or a single repeated character.
func CheckStrength(token string) error {
	if len(token) < MinTokenLength {
		return fmt.Errorf("token must be at least %d characters", MinTokenLength)
	}
	if len(token) > maxTokenLength {
		return fmt.Errorf("token must be at most %d characters", maxTokenLength)
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return errors.New("token must contain only printable non-space ASCII characters")
		}
	}
	if strings.Trim(token, string(token[0])) == "" {
		return errors.New("token must not repeat a single character")
	}
	return nil
}

// Check verifies the request's bearer token against the token file contents
// using a constant-time comparison.
func Check(r *http.Request, tokenFile string) error {
	want, err := LoadToken(tokenFile)
	if err != nil {
		return err
	}
	scheme, provided, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ErrUnauthorized
	}
	provided = strings.TrimSpace(provided)
	if len(provided) != len(want) || subtle.ConstantTimeCompare([]byte(provided), []byte(want)) != 1 {
		return ErrUnauthorized
	}
	return nil
}
