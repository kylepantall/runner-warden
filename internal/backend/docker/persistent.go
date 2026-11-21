package docker

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

// Persistent lists this instance's running persistent containers with their
// current memory. They sit in the job slice but outside the ledger, so the
// daemon reserves their memory as external.
func (b *Backend) Persistent(ctx context.Context) ([]core.PersistentContainer, error) {
	f := b.labelFilter("")
	f.Add("label", naming.MetaRole+"="+rolePersistent)
	cs, err := b.api.ContainerList(ctx, container.ListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]core.PersistentContainer, 0, len(cs))
	for _, c := range cs {
		pc := core.PersistentContainer{Name: c.ID}
		if len(c.Names) > 0 {
			pc.Name = strings.TrimPrefix(c.Names[0], "/")
		}
		if s, ok := b.sampleContainer(ctx, c.ID); ok {
			pc.Mem = s.memCurrent
		}
		out = append(out, pc)
	}
	return out, nil
}

// rolePersistent is the role label value of containers that outlive runners.
const rolePersistent = "persistent"
