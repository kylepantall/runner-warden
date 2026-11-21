package docker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/kylepantall/runner-warden/internal/naming"
)

// Cleanup removes everything a runner and its jobs created. Idempotent and safe
// for runners that never started. Resources are found by label only.
func (b *Backend) Cleanup(ctx context.Context, runner string) error {
	if !b.validName(runner) {
		// Never build a filesystem path from a name we could not have made.
		return errors.New("invalid runner name")
	}
	b.mu.Lock()
	if p := b.proxies[runner]; p != nil {
		_ = p.Close()
		delete(b.proxies, runner)
	}
	delete(b.stats, runner)
	b.mu.Unlock()

	var errs []error
	keep := func(err error) {
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	keep(b.stopProxyUnit(ctx, runner))

	f := b.labelFilter(runner)
	cs, err := b.api.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	keep(err)
	for _, c := range cs {
		if c.Labels[naming.MetaRole] == rolePersistent {
			continue // outlives its runner by design
		}
		keep(b.api.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}))
		b.forgetCgroup(c.ID)
	}
	ns, err := b.api.NetworkList(ctx, network.ListOptions{Filters: f})
	keep(err)
	for _, n := range ns {
		keep(b.api.NetworkRemove(ctx, n.ID))
	}
	vs, err := b.api.VolumeList(ctx, volume.ListOptions{Filters: f})
	keep(err)
	for _, v := range vs.Volumes {
		keep(b.api.VolumeRemove(ctx, v.Name, true))
	}

	keep(b.removeDir(ctx, runner))
	if err := os.Remove(b.proxySock(runner)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		b.log.Warn("runner cleanup incomplete", "runner", runner, "errors", len(errs), "first", errs[0].Error())
	}
	return errors.Join(errs...)
}

// removeDir deletes the runner dir. Job steps leave files owned by other uids;
// the daemon normally runs as root, but if removal is denied a root helper
// container does it.
func (b *Backend) removeDir(ctx context.Context, runner string) error {
	dir := b.runnerDir(runner)
	err := os.RemoveAll(dir)
	if err == nil || !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		return err
	}
	b.log.Info("runner dir removal denied, using helper container", "runner", runner)
	return b.removeDirViaHelper(ctx, runner)
}

func (b *Backend) removeDirViaHelper(ctx context.Context, runner string) error {
	cfg := &container.Config{
		Image:      b.cfg.Image,
		User:       "0",
		Entrypoint: []string{"/bin/sh", "-c", `rm -rf "/rw/$1"`, "runner-warden", runner},
	}
	hc := &container.HostConfig{
		Binds:     []string{bind(b.runnersDir(), "/rw", false)},
		Resources: container.Resources{CgroupParent: b.cfg.Slice},
	}
	created, err := b.api.ContainerCreate(ctx, cfg, hc, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return err
	}
	defer func() {
		_ = b.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()
	if err := b.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return err
	}
	waitC, errC := b.api.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case res := <-waitC:
		if res.StatusCode != 0 {
			return errors.New("helper rm exited non-zero")
		}
		return nil
	case err := <-errC:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
