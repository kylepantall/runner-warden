package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
)

// createPath matches create calls with or without an API version prefix.
var createPath = regexp.MustCompile(`^(/v[0-9.]+)?/(containers|networks|volumes)/create$`)

// dockerProxy is a per-runner Docker API proxy. It passes everything through
// (including hijacked attach/exec streams) and only rewrites create calls to
// stamp ownership labels and, for containers, a cgroup parent.
type dockerProxy struct {
	log      *slog.Logger
	runner   string
	slice    string
	upstream string
	sock     string
	srv      *http.Server
}

func startProxy(log *slog.Logger, sockPath, upstream, runner, slice string) (*dockerProxy, error) {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(sockPath, 0o666)
	p := &dockerProxy{log: log, runner: runner, slice: slice, upstream: upstream, sock: sockPath}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: "docker"})
			r.Out.Host = "docker"
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", upstream)
		}},
		FlushInterval: -1,
	}
	p.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if m := createPath.FindStringSubmatch(r.URL.Path); m != nil {
				if err := p.rewrite(r, m[2]); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
		}
		rp.ServeHTTP(w, r)
	})}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *dockerProxy) rewrite(r *http.Request, kind string) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	var doc map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &doc); err != nil {
			return err
		}
	} else {
		doc = map[string]any{}
	}
	labels, _ := doc["Labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	labels[ownerLabel] = "1"
	labels[ownerLabel+".runner"] = p.runner
	doc["Labels"] = labels
	if kind == "containers" {
		hc, _ := doc["HostConfig"].(map[string]any)
		if hc == nil {
			hc = map[string]any{}
		}
		hc["CgroupParent"] = p.slice
		hc["OomScoreAdj"] = 500
		// The runner bind-mounts /var/run/docker.sock into job containers. The
		// source resolves on the host, which would hand jobs the real socket
		// and bypass this proxy, so point it back at the proxy socket.
		if binds, ok := hc["Binds"].([]any); ok {
			for i, b := range binds {
				if s, _ := b.(string); strings.HasPrefix(s, "/var/run/docker.sock:") {
					binds[i] = p.sock + strings.TrimPrefix(s, "/var/run/docker.sock")
					p.log.Info("proxy redirected socket bind", "runner", p.runner, "from", s, "to", binds[i])
				}
			}
		}
		doc["HostConfig"] = hc
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	p.log.Info("proxy rewrote create", "runner", p.runner, "kind", kind, "image", doc["Image"])
	return nil
}

func (p *dockerProxy) close() { _ = p.srv.Close() }

// cleanupRunner removes every container, network and volume a runner's jobs
// created through its proxy, found by label only.
func cleanupRunner(ctx context.Context, log *slog.Logger, dc *dockerclient.Client, runner string) {
	f := filters.NewArgs(filters.Arg("label", ownerLabel+".runner="+runner))
	cs, _ := dc.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	for _, c := range cs {
		err := dc.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		log.Info("cleanup container", "runner", runner, "image", c.Image, "error", errStr(err))
	}
	ns, _ := dc.NetworkList(ctx, network.ListOptions{Filters: f})
	for _, n := range ns {
		err := dc.NetworkRemove(ctx, n.ID)
		log.Info("cleanup network", "runner", runner, "name", n.Name, "error", errStr(err))
	}
	vs, _ := dc.VolumeList(ctx, volume.ListOptions{Filters: f})
	for _, v := range vs.Volumes {
		err := dc.VolumeRemove(ctx, v.Name, true)
		log.Info("cleanup volume", "runner", runner, "name", v.Name, "error", errStr(err))
	}
}
