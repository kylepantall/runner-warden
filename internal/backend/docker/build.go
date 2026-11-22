package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/build"

	"github.com/kylepantall/runner-warden/internal/pulllock"
)

// build builds tag from a one-file context holding dockerfile. It holds the
// shared pull lock, like a pull, so image GC never runs mid-build.
func (b *Backend) build(ctx context.Context, tag, dockerfile string) error {
	if b.cfg.StateDir != "" {
		if err := mkdirAll(b.runnersDir()); err != nil {
			return err
		}
		l, err := pulllock.New(PullLockPath(b.cfg.StateDir)).RLock(ctx)
		if err != nil {
			return err
		}
		defer l.Close()
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	b.log.Info("building runner image", "image", tag)
	resp, err := b.api.ImageBuild(ctx, &buf, build.ImageBuildOptions{
		Tags: []string{tag}, Remove: true, ForceRemove: true, PullParent: false,
		Labels: map[string]string{"io.github.kylepantall.runner-warden.build": "1"},
	})
	if err != nil {
		return fmt.Errorf("build %s: %w", tag, err)
	}
	defer resp.Body.Close()
	// The build streams JSON messages; a failure arrives as one with error.
	dec := json.NewDecoder(resp.Body)
	var last string
	for {
		var m struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("build %s: %w", tag, err)
		}
		if m.Error != "" {
			return fmt.Errorf("build %s: %s", tag, m.Error)
		}
		if m.Stream != "" {
			last = m.Stream
		}
	}
	if _, err := b.api.ImageInspect(ctx, tag); err != nil {
		return fmt.Errorf("build %s: no image after the build (%s): %w", tag, last, err)
	}
	return nil
}
