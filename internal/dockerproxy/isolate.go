package dockerproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/naming"
)

// Routes that need isolation. The daemon is shared by every runner, so a job
// must only reach containers (and their exec sessions, networks and volumes)
// that this runner's proxy created for it.
var (
	containerPath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/([^/]+)(/.*)?$`)
	execPath      = regexp.MustCompile(`^(/v[0-9.]+)?/exec/([^/]+)(/.*)?$`)
	networkPath   = regexp.MustCompile(`^(/v[0-9.]+)?/networks/([^/]+)(/.*)?$`)
	volumePath    = regexp.MustCompile(`^(/v[0-9.]+)?/volumes/([^/]+)(/.*)?$`)
	commitPath    = regexp.MustCompile(`^(/v[0-9.]+)?/commit$`)
	listPath      = regexp.MustCompile(`^(/v[0-9.]+)?/(containers/json|volumes|networks)$`)
	// containerListPath is listPath's container case, post-filtered by the proxy.
	containerListPath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/json$`)
	prunePath         = regexp.MustCompile(`^(/v[0-9.]+)?/(containers|volumes|networks|images)/prune$`)
)

const ownedCacheTTL = 5 * time.Second

type ownedEntry struct {
	ok   bool
	when time.Time
}

// authorize enforces ownership on one request. It returns 0 to proceed, or an
// HTTP status and error to refuse with.
func (p *Proxy) authorize(r *http.Request) (int, error) {
	pth := r.URL.Path
	if listPath.MatchString(pth) && r.Method == http.MethodGet || prunePath.MatchString(pth) && r.Method == http.MethodPost {
		labels := p.Labels()
		if containerListPath.MatchString(pth) {
			// Filters cannot OR labels: ask for the whole instance and let
			// serveContainerList keep what this runner may see.
			labels = map[string]string{naming.MetaManaged: "1", naming.MetaInstance: p.cfg.Instance}
		}
		if err := p.scopeFilters(r, labels); err != nil {
			return http.StatusBadRequest, err
		}
		return 0, nil
	}
	if commitPath.MatchString(pth) {
		return p.requireOwned(r.Context(), "containers", r.URL.Query().Get("container"))
	}
	if m := containerPath.FindStringSubmatch(pth); m != nil {
		switch m[2] {
		case "create", "json", "prune":
			return 0, nil
		}
		return p.requireOwned(r.Context(), "containers", m[2])
	}
	if m := execPath.FindStringSubmatch(pth); m != nil {
		cid, err := p.execContainer(r.Context(), m[2])
		if err != nil {
			return http.StatusForbidden, errors.New("exec session not allowed")
		}
		return p.requireOwned(r.Context(), "containers", cid)
	}
	// Networks and volumes may be inspected, but only ours changed.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if m := networkPath.FindStringSubmatch(pth); m != nil && m[2] != "create" && m[2] != "prune" {
			return p.requireOwned(r.Context(), "networks", m[2])
		}
		if m := volumePath.FindStringSubmatch(pth); m != nil && m[2] != "create" && m[2] != "prune" {
			return p.requireOwned(r.Context(), "volumes", m[2])
		}
	}
	return 0, nil
}

// scopeFilters ANDs the given labels into the request's filters.
func (p *Proxy) scopeFilters(r *http.Request, labels map[string]string) error {
	q := r.URL.Query()
	f := map[string][]string{}
	if raw := q.Get("filters"); raw != "" {
		var legacy map[string]map[string]bool
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			f = map[string][]string{}
			if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
				return errors.New("malformed filters parameter")
			}
			for k, vs := range legacy {
				for v, on := range vs {
					if on {
						f[k] = append(f[k], v)
					}
				}
			}
		}
	}
	for k, v := range labels {
		f["label"] = append(f["label"], k+"="+v)
	}
	out, err := json.Marshal(f)
	if err != nil {
		return err
	}
	q.Set("filters", string(out))
	r.URL.RawQuery = q.Encode()
	return nil
}

// ownsLabels reports whether labels mark a job resource of this runner, or
// (containers only) a persistent container of this instance.
func (p *Proxy) ownsLabels(kind string, l map[string]string) bool {
	if kind == "containers" && l[naming.MetaRole] == rolePersistent &&
		l[naming.MetaManaged] == "1" && l[naming.MetaInstance] == p.cfg.Instance {
		return true
	}
	for k, v := range p.Labels() {
		if l[k] != v {
			return false
		}
	}
	return true
}

// maxListBody bounds the container list the proxy buffers to post-filter.
const maxListBody = 32 << 20

// serveContainerList answers GET /containers/json with only the containers
// this runner may use: its own jobs plus the instance's persistent ones.
func (p *Proxy) serveContainerList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit > 0 {
		// Applied after filtering, or other runners' containers would eat it.
		q.Del("limit")
		r.URL.RawQuery = q.Encode()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+r.URL.RequestURI(), nil) //nolint:gosec // local docker socket upstream, path built by the proxy
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp, err := p.up.Do(req) //nolint:gosec // upstream is the local docker socket
	if err != nil {
		p.log.Warn("proxy upstream error", "runner", p.cfg.Runner, "error", err.Error())
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBody+1))
	if err != nil || len(body) > maxListBody {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if resp.StatusCode == http.StatusOK {
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		kept := make([]json.RawMessage, 0, len(items))
		for _, it := range items {
			var s struct{ Labels map[string]string }
			if json.Unmarshal(it, &s) == nil && p.ownsLabels("containers", s.Labels) {
				kept = append(kept, it)
			}
		}
		if limit > 0 && len(kept) > limit {
			kept = kept[:limit]
		}
		if body, err = json.Marshal(kept); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	for k, vs := range resp.Header {
		if k == "Content-Length" || k == "Content-Encoding" {
			continue
		}
		w.Header()[k] = vs
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (p *Proxy) requireOwned(ctx context.Context, kind, id string) (int, error) {
	if id == "" {
		return http.StatusForbidden, errors.New("resource not allowed")
	}
	key := kind + "/" + id
	p.mu.Lock()
	if e, ok := p.owned[key]; ok && time.Since(e.when) < ownedCacheTTL {
		p.mu.Unlock()
		if e.ok {
			return 0, nil
		}
		return http.StatusForbidden, errors.New("resource not allowed")
	}
	p.mu.Unlock()
	ok, err := p.inspectOwned(ctx, kind, id)
	if err != nil {
		return http.StatusBadGateway, err
	}
	p.mu.Lock()
	if p.owned == nil {
		p.owned = map[string]ownedEntry{}
	}
	if len(p.owned) > 1024 {
		p.owned = map[string]ownedEntry{}
	}
	p.owned[key] = ownedEntry{ok: ok, when: time.Now()}
	p.mu.Unlock()
	if !ok {
		return http.StatusForbidden, errors.New("resource not allowed")
	}
	return 0, nil
}

// forget drops cached ownership (after a delete the name may be reused).
func (p *Proxy) forget() {
	p.mu.Lock()
	p.owned = nil
	p.mu.Unlock()
}

// inspectOwned reads the resource's labels from the real daemon. A resource
// that does not exist is "owned": the daemon answers 404 itself.
func (p *Proxy) inspectOwned(ctx context.Context, kind, id string) (bool, error) {
	target := "/" + kind + "/" + url.PathEscape(id)
	if kind == "containers" {
		target += "/json"
	}
	body, status, err := p.upstreamGet(ctx, target)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return true, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("inspect status %d", status)
	}
	var doc struct {
		Labels map[string]string `json:"Labels"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, err
	}
	labels := doc.Labels
	if kind == "containers" {
		labels = doc.Config.Labels
	}
	return p.ownsLabels(kind, labels), nil
}

func (p *Proxy) execContainer(ctx context.Context, id string) (string, error) {
	body, status, err := p.upstreamGet(ctx, "/exec/"+url.PathEscape(id)+"/json")
	if err != nil || status != http.StatusOK {
		return "", errors.New("exec inspect failed")
	}
	var doc struct{ ContainerID string }
	if err := json.Unmarshal(body, &doc); err != nil || doc.ContainerID == "" {
		return "", errors.New("exec inspect failed")
	}
	return doc.ContainerID, nil
}

func (p *Proxy) upstreamGet(ctx context.Context, pth string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+pth, nil) //nolint:gosec // local docker socket upstream, path built by the proxy
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.up.Do(req) //nolint:gosec // upstream is the local docker socket; the path is built by the proxy
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return b, resp.StatusCode, err
}

// --- bind allowlist ---

// bindAllowed reports whether a host source may be bind-mounted into a job
// container. Named volumes (no leading slash) and the real Docker socket
// (rewritten to the proxy socket) are fine; host paths must sit under the
// runner dir, a cache path or an operator-allowed path.
func (p *Proxy) bindAllowed(src string) bool {
	if !strings.HasPrefix(src, "/") || isHostSocket(src) || src == p.cfg.Socket {
		return true
	}
	c := path.Clean(src)
	for _, a := range p.cfg.AllowedBinds {
		a = path.Clean(a)
		if a == "/" {
			continue // never allow the host root, even if configured
		}
		if c == a || strings.HasPrefix(c, a+"/") {
			return true
		}
	}
	return false
}

// checkContainerRefs vets a create body's host paths and cross-container
// references. It returns an error to refuse with 403.
func (p *Proxy) checkContainerRefs(ctx context.Context, doc map[string]any) error {
	hc, _ := doc["HostConfig"].(map[string]any)
	if hc == nil {
		return nil
	}
	binds, _ := hc["Binds"].([]any)
	for _, b := range binds {
		s, _ := b.(string)
		src, _, _ := strings.Cut(s, ":")
		if !p.bindAllowed(src) {
			return errors.New("bind mount source not allowed")
		}
	}
	mounts, _ := hc["Mounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if t, _ := mm["Type"].(string); t == "bind" {
			if src, _ := mm["Source"].(string); !p.bindAllowed(src) {
				return errors.New("bind mount source not allowed")
			}
		}
	}
	vf, _ := hc["VolumesFrom"].([]any)
	for _, v := range vf {
		s, _ := v.(string)
		id, _, _ := strings.Cut(s, ":")
		if st, err := p.requireOwned(ctx, "containers", id); st != 0 {
			return err
		}
	}
	if nm, _ := hc["NetworkMode"].(string); strings.HasPrefix(nm, "container:") {
		if st, err := p.requireOwned(ctx, "containers", strings.TrimPrefix(nm, "container:")); st != 0 {
			return err
		}
	}
	return nil
}
