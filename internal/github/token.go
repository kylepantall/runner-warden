// Package gh talks to GitHub: scale-set API, listener, and REST helpers.
package gh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Token errors.
var (
	ErrTokenPerms = errors.New("token file is group or world accessible")
	ErrTokenEmpty = errors.New("token file is empty")
)

// Token reads a token file, trimming whitespace. It refuses files readable by
// group or others so a leaked mode never silently exposes the credential.
func Token(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("open token file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat token file: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: %s has mode %04o", ErrTokenPerms, file, st.Mode().Perm())
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", ErrTokenEmpty
	}
	return t, nil
}
