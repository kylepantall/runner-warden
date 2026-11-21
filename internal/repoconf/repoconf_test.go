package repoconf

import "testing"

func TestImageFor(t *testing.T) {
	f, err := Parse([]byte(`
image: ghcr.io/acme/ci:1
jobs:
  - match: ci.yml/android*
    image: ghcr.io/acme/android:1
  - match: "*/build / linux"
    image: ghcr.io/acme/linux:1
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ wf, job, want string }{
		{"ci.yml", "android (2/2)", "ghcr.io/acme/android:1"},
		{"release.yml", "android", "ghcr.io/acme/ci:1"},
		{"x.yml", "build / linux", "ghcr.io/acme/linux:1"},
		{"ci.yml", "test", "ghcr.io/acme/ci:1"},
	} {
		if got := f.ImageFor(c.wf, c.job); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.wf, c.job, got, c.want)
		}
	}
	var none *File
	if none.ImageFor("a", "b") != "" {
		t.Fatal("nil file")
	}
}

func TestParseEmptyAndJobsOnly(t *testing.T) {
	f, err := Parse(nil)
	if err != nil || f.ImageFor("a", "b") != "" {
		t.Fatal(f, err)
	}
	f, err = Parse([]byte("jobs:\n  - match: a.yml/*\n    image: x:1\n"))
	if err != nil || f.ImageFor("b.yml", "j") != "" || f.ImageFor("a.yml", "j") != "x:1" {
		t.Fatal(f, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"imgae: x:1\n",     // typo: unknown key
		"image: \"-x\"\n",  // flag-like
		"image: \"a b\"\n", // whitespace
		"jobs:\n  - match: nojob\n    image: x\n",   // no workflow/job split
		"jobs:\n  - match: a.yml/[\n    image: x\n", // bad glob
		"jobs:\n  - match: a.yml/*\n",               // missing image
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	if _, err := Parse(make([]byte, maxSize+1)); err == nil {
		t.Error("accepted oversized file")
	}
}

func TestAllowed(t *testing.T) {
	pats := []string{"ghcr.io/acme/*", "docker.io/library/ubuntu:24.04"}
	for img, want := range map[string]bool{
		"ghcr.io/acme/ci:1":              true,
		"ghcr.io/acme/team/ci:1":         false, // * stops at /
		"ghcr.io/evil/ci:1":              false,
		"docker.io/library/ubuntu:24.04": true,
	} {
		if Allowed(img, pats) != want {
			t.Errorf("%s: want %v", img, want)
		}
	}
	if Allowed("x", nil) {
		t.Error("empty allowlist must deny")
	}
}
