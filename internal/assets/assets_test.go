package assets

import (
	"bytes"
	"os"
	"testing"
)

func TestUnitMatchesPackaging(t *testing.T) {
	b, err := os.ReadFile("../../packaging/systemd/runner-warden@.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, SystemdUnit) {
		t.Fatal("internal/assets/runner-warden@.service differs from packaging/systemd; copy it")
	}
}
