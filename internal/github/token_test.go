package gh

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got, err := Token(write("ok", " tok\n", 0o600)); err != nil || got != "tok" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := Token(write("open", "tok", 0o640)); !errors.Is(err, ErrTokenPerms) {
		t.Fatalf("want ErrTokenPerms, got %v", err)
	}
	if _, err := Token(write("world", "tok", 0o604)); !errors.Is(err, ErrTokenPerms) {
		t.Fatalf("want ErrTokenPerms, got %v", err)
	}
	if _, err := Token(write("empty", "\n", 0o600)); !errors.Is(err, ErrTokenEmpty) {
		t.Fatalf("want ErrTokenEmpty, got %v", err)
	}
	if _, err := Token(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("want error")
	}
}
