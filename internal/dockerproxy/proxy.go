// Package dockerproxy is the per-runner Docker API proxy. It passes every
// request through (including hijacked attach/exec streams and builds) and only
// rewrites create calls so that everything a job creates is labelled, placed in
// the instance cgroup slice and wired to this proxy instead of the real socket.
package dockerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kylepantall/runner-warden/internal/naming"
)

// DefaultMaxBody bounds the request bodies the proxy buffers for rewriting.
const DefaultMaxBody = 10 << 20

// Real Docker socket paths a runner may bind into a job container.
var hostSockets = []string{"/var/run/docker.sock", "/run/docker.sock"}

var (
	createPath = regexp.MustCompile(`^(/v[0-9.]+)?/(containers|networks|volumes)/create$`)
	pullPath   = regexp.MustCompile(`^(/v[0-9.]+)?/images/create$`)
	buildPath  = regexp.MustCompile(`^(/v[0-9.]+)?/build$`)
	// Cluster-level endpoints a job has no business with: swarm services and
	// secrets could mint resources outside the labelled, slice-capped path.
	deniedPath = regexp.MustCompile(`^(/v[0-9.]+)?/(swarm|services|tasks|plugins|secrets|configs|nodes)(/|$)`)
)

// Config configures one proxy.
type Config struct {
	Socket      string            // unix socket the proxy listens on
	Upstream    string            // real Docker socket
	Runner      string            // runner name stamped on everything
	Instance    string            // instance stamped on everything
	HostID      string            // host id stamped on everything
	Slice       string            // HostConfig.CgroupParent for job containers
	OOMScoreAdj int               // HostConfig.OomScoreAdj for job containers
	CacheBinds  []string          // "host:container[:opts]" appended to job containers
	CacheEnv    map[string]string // env appended to job containers
	// AllowedBinds are host paths (and everything under them) a job may
	// bind-mount; anything else outside named volumes is refused.
	AllowedBinds []string
	MaxBody      int64        // 0 = DefaultMaxBody
	Log          *slog.Logger // nil = discard
}

// Proxy is a running per-runner proxy.
type Proxy struct {
	cfg Config
	log *slog.Logger
	srv *http.Server
	rp  *httputil.ReverseProxy
	up  *http.Client // inspect calls to the real daemon

	mu       sync.Mutex
	pauses   int
	resumeCh chan struct{} // closed when pauses drops to 0; non-nil while paused
	inflight int
	owned    map[string]ownedEntry
	closed   chan struct{}
	once     sync.Once
}

// New starts a proxy listening on cfg.Socket.
func New(cfg Config) (*Proxy, error) {
	if cfg.Socket == "" || cfg.Upstream == "" {
		return nil, errors.New("dockerproxy: socket and upstream are required")
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	_ = os.Remove(cfg.Socket)
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Socket, 0o666); err != nil { //nolint:gosec // the socket is deliberately world-connectable; access is gated per request
		_ = ln.Close()
		return nil, err
	}
	p := &Proxy{cfg: cfg, log: log, closed: make(chan struct{})}
	upstream := cfg.Upstream
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", upstream)
	}}
	p.up = &http.Client{Transport: tr}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: "docker"})
			r.Out.Host = "docker"
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Warn("proxy upstream error", "runner", cfg.Runner, "error", err.Error())
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// Socket returns the proxy's socket path.
func (p *Proxy) Socket() string { return p.cfg.Socket }

// Close stops the proxy, releases blocked pulls and removes the socket.
func (p *Proxy) Close() error {
	p.once.Do(func() { close(p.closed) })
	err := p.srv.Close()
	_ = os.Remove(p.cfg.Socket)
	return err
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if deniedPath.MatchString(r.URL.Path) {
		p.log.Warn("proxy denied endpoint", "runner", p.cfg.Runner)
		http.Error(w, "endpoint not allowed", http.StatusForbidden)
		return
	}
	if status, err := p.authorize(r); status != 0 {
		p.log.Warn("proxy refused request", "runner", p.cfg.Runner, "status", status)
		http.Error(w, err.Error(), status)
		return
	}
	if r.Method == http.MethodDelete {
		defer p.forget()
	}
	if r.Method == http.MethodPost && buildPath.MatchString(r.URL.Path) {
		if err := p.rewriteBuild(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if r.Method == http.MethodPost {
		if m := createPath.FindStringSubmatch(r.URL.Path); m != nil {
			if status, err := p.rewrite(w, r, m[2]); err != nil {
				p.log.Warn("proxy rejected create", "runner", p.cfg.Runner, "kind", m[2], "status", status, "error", err.Error())
				http.Error(w, err.Error(), status)
				return
			}
		} else if pullPath.MatchString(r.URL.Path) {
			if !p.beginPull(r.Context()) {
				http.Error(w, "proxy closing", http.StatusServiceUnavailable)
				return
			}
			defer p.endPull()
		}
	}
	p.rp.ServeHTTP(w, r)
}

// --- pull gate ---

// PausePulls blocks new image pulls (they wait, they do not fail). Calls nest.
func (p *Proxy) PausePulls() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pauses == 0 {
		p.resumeCh = make(chan struct{})
	}
	p.pauses++
}

// ResumePulls undoes one PausePulls; pulls proceed when the last is undone.
func (p *Proxy) ResumePulls() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pauses == 0 {
		return
	}
	p.pauses--
	if p.pauses == 0 {
		close(p.resumeCh)
		p.resumeCh = nil
	}
}

// InFlightPulls is the number of image pulls currently proxied.
func (p *Proxy) InFlightPulls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight
}

// Gate adapts the proxy to the Pause/Resume/InFlight shape used by image GC.
func (p *Proxy) Gate() Gate { return Gate{p} }

// Gate is a Pause/Resume/InFlight view of a proxy.
type Gate struct{ p *Proxy }

// Pause stops new image pulls.
func (g Gate) Pause() { g.p.PausePulls() }

// Resume allows image pulls again.
func (g Gate) Resume() { g.p.ResumePulls() }

// InFlight is the number of pulls in progress.
func (g Gate) InFlight() int { return g.p.InFlightPulls() }

func (p *Proxy) beginPull(ctx context.Context) bool {
	for {
		p.mu.Lock()
		ch := p.resumeCh
		if ch == nil {
			p.inflight++
			p.mu.Unlock()
			return true
		}
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		case <-p.closed:
			return false
		}
	}
}

func (p *Proxy) endPull() {
	p.mu.Lock()
	p.inflight--
	p.mu.Unlock()
}

// --- create rewriting ---

func (p *Proxy) rewrite(w http.ResponseWriter, r *http.Request, kind string) (int, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return http.StatusRequestEntityTooLarge, errors.New("request body too large")
		}
		return http.StatusBadRequest, err
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
			return http.StatusBadRequest, errors.New("malformed JSON body")
		}
	}
	if kind == "containers" {
		if err := p.checkContainerRefs(r.Context(), doc); err != nil {
			return http.StatusForbidden, err
		}
	}
	doc["Labels"] = p.stampLabels(doc["Labels"], "job")
	if kind == "containers" {
		p.rewriteContainer(doc)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	r.TransferEncoding = nil
	return 0, nil
}

// rewriteBuild makes built images attributable and keeps a job from setting
// labels under our metadata namespace: client-supplied keys there are dropped
// and the proxy's own labels (role=job) are stamped on.
func (p *Proxy) rewriteBuild(r *http.Request) error {
	q := r.URL.Query()
	labels := map[string]string{}
	for _, raw := range q["labels"] {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return errors.New("malformed labels parameter")
		}
		for k, v := range m {
			if !strings.HasPrefix(strings.ToLower(k), naming.MetaPrefix) {
				labels[k] = v
			}
		}
	}
	for k, v := range p.Labels() {
		labels[k] = v
	}
	out, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	q.Set("labels", string(out))
	r.URL.RawQuery = q.Encode()
	return nil
}

// Labels returns the ownership labels the proxy stamps (role=job).
func (p *Proxy) Labels() map[string]string {
	return map[string]string{
		naming.MetaManaged:  "1",
		naming.MetaInstance: p.cfg.Instance,
		naming.MetaHostID:   p.cfg.HostID,
		naming.MetaRunner:   p.cfg.Runner,
		naming.MetaRole:     "job",
	}
}

func (p *Proxy) stampLabels(existing any, role string) map[string]any {
	labels, _ := existing.(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	for k, v := range p.Labels() {
		labels[k] = v
	}
	labels[naming.MetaRole] = role
	return labels
}

func (p *Proxy) rewriteContainer(doc map[string]any) {
	hc, _ := doc["HostConfig"].(map[string]any)
	if hc == nil {
		hc = map[string]any{}
	}
	hc["CgroupParent"] = p.cfg.Slice
	hc["OomScoreAdj"] = p.cfg.OOMScoreAdj

	binds, _ := hc["Binds"].([]any)
	for i, b := range binds {
		if s, ok := b.(string); ok {
			binds[i] = p.redirectBind(s)
		}
	}
	mounts, _ := hc["Mounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if src, _ := mm["Source"].(string); isHostSocket(src) {
			mm["Source"] = p.cfg.Socket
		}
	}

	// Cache binds, unless the job already uses that target path.
	used := usedTargets(binds, mounts, doc["Volumes"])
	for _, cb := range p.cfg.CacheBinds {
		target := bindTarget(cb)
		if target == "" || used[target] {
			continue
		}
		used[target] = true
		binds = append(binds, cb)
	}
	if binds != nil {
		hc["Binds"] = binds
	}
	doc["HostConfig"] = hc

	// Cache env, unless the job already sets that variable.
	env, _ := doc["Env"].([]any)
	set := map[string]bool{}
	for _, e := range env {
		if s, ok := e.(string); ok {
			k, _, _ := strings.Cut(s, "=")
			set[k] = true
		}
	}
	keys := make([]string, 0, len(p.cfg.CacheEnv))
	for k := range p.cfg.CacheEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !set[k] {
			env = append(env, k+"="+p.cfg.CacheEnv[k])
		}
	}
	if env != nil {
		doc["Env"] = env
	}
}

func isHostSocket(p string) bool {
	for _, s := range hostSockets {
		if p == s {
			return true
		}
	}
	return false
}

// redirectBind points a bind whose source is the real Docker socket at the
// proxy socket (the daemon resolves bind sources on the host).
func (p *Proxy) redirectBind(b string) string {
	src, rest, ok := strings.Cut(b, ":")
	if ok && isHostSocket(src) {
		return fmt.Sprintf("%s:%s", p.cfg.Socket, rest)
	}
	return b
}

// bindTarget returns the container path of a "src:dst[:opts]" bind.
func bindTarget(b string) string {
	parts := strings.Split(b, ":")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func usedTargets(binds, mounts []any, volumes any) map[string]bool {
	used := map[string]bool{}
	for _, b := range binds {
		if s, ok := b.(string); ok {
			if t := bindTarget(s); t != "" {
				used[t] = true
			}
		}
	}
	for _, m := range mounts {
		if mm, ok := m.(map[string]any); ok {
			if t, _ := mm["Target"].(string); t != "" {
				used[t] = true
			}
		}
	}
	if vm, ok := volumes.(map[string]any); ok {
		for t := range vm {
			used[t] = true
		}
	}
	return used
}
