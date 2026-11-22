package linuxhost

import (
	"context"
	"errors"
	"runtime"

	"github.com/kylepantall/runner-warden/internal/core"
)

// AgentBackend is what an agent drives on its host (the Docker backend).
type AgentBackend interface {
	core.Backend
	EnsureImage(ctx context.Context, ref string) error
	Adopt(ctx context.Context, h core.Handle) error
}

// Dispatch runs one request against b. Failures are reported in the
// response, never as a crash, so the daemon can tell them from SSH errors.
func Dispatch(ctx context.Context, b AgentBackend, req Request, version string) Response {
	resp := Response{Version: version, Arch: runtime.GOARCH}
	if err := dispatch(ctx, b, req, &resp); err != nil {
		resp.Error = err.Error()
	}
	return resp
}

func dispatch(ctx context.Context, b AgentBackend, req Request, resp *Response) error {
	switch req.Op {
	case OpPing:
		return nil
	case OpPrepare:
		return b.Prepare(ctx)
	case OpStart:
		if req.Spec == nil {
			return errors.New("start: no spec")
		}
		spec := *req.Spec
		if spec.Image != "" {
			if err := b.EnsureImage(ctx, spec.Image); err != nil {
				spec.Image = "" // the default image, as on the daemon's host
			}
		}
		h, err := b.Start(ctx, spec)
		resp.Handle = h
		return err
	case OpStats:
		if req.Handle == nil {
			return errors.New("stats: no handle")
		}
		u, err := b.Stats(ctx, *req.Handle)
		resp.Usage = u
		return err
	case OpStop:
		if req.Handle == nil {
			return errors.New("stop: no handle")
		}
		return b.Stop(ctx, *req.Handle, req.Grace)
	case OpCleanup:
		return b.Cleanup(ctx, req.Runner)
	case OpList:
		hs, err := b.List(ctx)
		resp.Handles = hs
		return err
	case OpAdopt:
		if req.Handle == nil {
			return errors.New("adopt: no handle")
		}
		return b.Adopt(ctx, *req.Handle)
	}
	return errors.New("unknown op " + req.Op)
}
