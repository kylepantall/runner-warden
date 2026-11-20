package winvm

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"
)

func TestEncodePS(t *testing.T) {
	b, err := base64.StdEncoding.DecodeString(encodePS("é'x"))
	if err != nil || len(b)%2 != 0 {
		t.Fatal(err)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	if string(utf16.Decode(u)) != "é'x" {
		t.Fatal("round trip")
	}
	if _, err := shellCommand("pwsh", strings.Repeat("x", 20000)); err == nil {
		t.Fatal("oversized script must be refused")
	}
	c, _ := shellCommand("pwsh", "x")
	if !strings.HasPrefix(c, "pwsh -NoProfile -NonInteractive -EncodedCommand ") {
		t.Fatal(c)
	}
}

func TestEnsureKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "key")
	s1, pub1, err := EnsureKey(p)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	if b, _ := os.ReadFile(p + ".pub"); strings.TrimSpace(string(b)) != pub1 { //nolint:gosec // test path under a temp dir
		t.Fatal("pub file")
	}
	s2, pub2, err := EnsureKey(p)
	if err != nil || pub2 != pub1 || string(s1.PublicKey().Marshal()) != string(s2.PublicKey().Marshal()) {
		t.Fatal("reload must keep the key")
	}
}

func newPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestTOFU(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "state", "known_hosts")
	cb := tofuCallback(kh)
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}
	k1, k2 := newPub(t), newPub(t)
	if err := cb("127.0.0.1:2222", addr, k1); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := cb("127.0.0.1:2222", addr, k1); err != nil {
		t.Fatalf("same key: %v", err)
	}
	if err := cb("127.0.0.1:2222", addr, k2); err == nil {
		t.Fatal("changed key must be refused")
	}
	if err := cb("127.0.0.1:2223", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2223}, k2); err != nil {
		t.Fatalf("other host: %v", err)
	}
}
