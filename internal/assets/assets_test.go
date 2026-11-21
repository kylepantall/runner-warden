package assets

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestUnitsMatchPackaging(t *testing.T) {
	files, err := filepath.Glob("../../packaging/systemd/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(Units) {
		t.Fatalf("packaging/systemd has %d files, Units has %d", len(files), len(Units))
	}
	for _, p := range files {
		b, err := os.ReadFile(p) //nolint:gosec // repo files from a fixed glob
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := Units[filepath.Base(p)]; !ok || !bytes.Equal(b, got) {
			t.Errorf("internal/assets/%s differs from packaging/systemd; copy it", filepath.Base(p))
		}
	}
}
