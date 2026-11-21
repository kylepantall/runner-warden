// Package repoconf reads a repo's .github/runner-warden.yml, which lets a
// project pick the runner image its jobs run in:
//
//	image: ghcr.io/acme/ci:1          # every job in this repo
//	jobs:
//	  - match: ci.yml/android*        # workflow-file/job-name, path.Match globs
//	    image: ghcr.io/acme/android-ci:1
//
// The first matching jobs entry wins, then image. The host decides which
// images are acceptable (linux.allowed_images).
package repoconf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Path is where the file lives in a repo.
const Path = ".github/runner-warden.yml"

// maxSize caps what Parse accepts; the file is a handful of lines.
const maxSize = 64 << 10

// File is the parsed .github/runner-warden.yml.
type File struct {
	Image string     `yaml:"image"`
	Jobs  []JobImage `yaml:"jobs"`
}

// JobImage picks an image for jobs matching "workflow-file/job-name".
type JobImage struct {
	Match string `yaml:"match"`
	Image string `yaml:"image"`
}

// Parse decodes and validates the file; an empty file means no settings.
// Unknown keys are errors so typos do not silently fall back to the default
// image.
func Parse(b []byte) (*File, error) {
	if len(b) > maxSize {
		return nil, errors.New("runner-warden.yml: file too large")
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("runner-warden.yml: %w", err)
	}
	if err := validImage(f.Image, true); err != nil {
		return nil, err
	}
	for i, j := range f.Jobs {
		if _, _, ok := split(j.Match); !ok {
			return nil, fmt.Errorf("runner-warden.yml: jobs[%d].match %q: want workflow-file/job-name", i, j.Match)
		}
		if err := validImage(j.Image, false); err != nil {
			return nil, fmt.Errorf("runner-warden.yml: jobs[%d]: %w", i, err)
		}
	}
	return &f, nil
}

func validImage(img string, optional bool) error {
	if img == "" {
		if optional {
			return nil
		}
		return errors.New("image is required")
	}
	if strings.ContainsAny(img, " \t\n") || strings.HasPrefix(img, "-") {
		return fmt.Errorf("image %q: not an image reference", img)
	}
	return nil
}

// split cuts "workflow/job" at the first "/" (job names may contain "/")
// and checks both globs.
func split(m string) (wf, job string, ok bool) {
	wf, job, ok = strings.Cut(m, "/")
	if !ok || wf == "" || job == "" {
		return "", "", false
	}
	if _, err := path.Match(wf, ""); err != nil {
		return "", "", false
	}
	if _, err := path.Match(job, ""); err != nil {
		return "", "", false
	}
	return wf, job, true
}

// ImageFor returns the image for a job, "" when the file names none.
func (f *File) ImageFor(workflowFile, job string) string {
	if f == nil {
		return ""
	}
	for _, j := range f.Jobs {
		wf, jb, _ := split(j.Match)
		if ok, _ := path.Match(wf, workflowFile); !ok {
			continue
		}
		// '*' must cross '/' inside job names, and a literal '/' in the
		// pattern still matches one in the name.
		if ok, _ := path.Match(strings.ReplaceAll(jb, "/", "\x00"), strings.ReplaceAll(job, "/", "\x00")); ok {
			return j.Image
		}
	}
	return f.Image
}

// Allowed reports whether img matches one of the host's glob patterns.
func Allowed(img string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, img); ok {
			return true
		}
	}
	return false
}
