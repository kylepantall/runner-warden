package dockerproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/naming"
)

type captured struct {
	mu   sync.Mutex
	path string
	body []byte
}

func (c *captured) set(p string, b []byte) { c.mu.Lock(); c.path, c.body = p, b; c.mu.Unlock() }
func (c *captured) get() (string, map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(c.body, &m)
	return c.path, m
}

// sockDir is short: unix socket paths are length-limited on macOS.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func startUpstream(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(sockDir(t), "up.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

func newProxy(t *testing.T, up string, mod func(*Config)) *Proxy {
	t.Helper()
	cfg := Config{
		Socket: filepath.Join(sockDir(t), "p.sock"), Upstream: up,
		Runner: "r1", Instance: "inst", HostID: "h1", Slice: "rw.slice", OOMScoreAdj: 500,
		CacheBinds: []string{"/cache/npm:/root/.npm"},
		CacheEnv:   map[string]string{"NPM_CACHE": "/root/.npm"},
	}
	if mod != nil {
		mod(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func client(p *Proxy) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p.Socket())
	}}}
}

func post(t *testing.T, p *Proxy, path, body string) *http.Response {
	t.Helper()
	resp, err := client(p).Post("http://docker"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func echoUpstream(c *captured) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.set(r.URL.Path, b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"x"}`))
	})
}

func TestContainerCreateRewrite(t *testing.T) {
	c := &captured{}
	p := newProxy(t, startUpstream(t, echoUpstream(c)), func(c *Config) { c.AllowedBinds = []string{"/w"} })
	body := `{"Image":"alpine","Labels":{"a":"b"},"Env":["FOO=1"],"HostConfig":{"Binds":["/var/run/docker.sock:/var/run/docker.sock","/run/docker.sock:/other.sock:ro","/w:/w"],"Memory":0}}`
	resp := post(t, p, "/v1.47/containers/create?name=x", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	path, doc := c.get()
	if path != "/v1.47/containers/create" {
		t.Fatalf("path %q", path)
	}
	labels := doc["Labels"].(map[string]any)
	want := map[string]string{
		naming.MetaManaged: "1", naming.MetaInstance: "inst", naming.MetaHostID: "h1",
		naming.MetaRunner: "r1", naming.MetaRole: "job", "a": "b",
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %s = %v want %s", k, labels[k], v)
		}
	}
	hc := doc["HostConfig"].(map[string]any)
	if hc["CgroupParent"] != "rw.slice" || hc["OomScoreAdj"] != float64(500) {
		t.Errorf("hostconfig %v", hc)
	}
	if _, ok := hc["Memory"]; !ok || hc["Memory"] != float64(0) {
		t.Errorf("job memory must be untouched: %v", hc["Memory"])
	}
	binds := hc["Binds"].([]any)
	wantBinds := []string{p.Socket() + ":/var/run/docker.sock", p.Socket() + ":/other.sock:ro", "/w:/w", "/cache/npm:/root/.npm"}
	if len(binds) != len(wantBinds) {
		t.Fatalf("binds %v", binds)
	}
	for i, b := range wantBinds {
		if binds[i] != b {
			t.Errorf("bind %d = %v want %s", i, binds[i], b)
		}
	}
	env := doc["Env"].([]any)
	if len(env) != 2 || env[1] != "NPM_CACHE=/root/.npm" {
		t.Errorf("env %v", env)
	}
}

func TestCacheNotDuplicatedWhenJobSetsIt(t *testing.T) {
	c := &captured{}
	p := newProxy(t, startUpstream(t, echoUpstream(c)), nil)
	post(t, p, "/containers/create", `{"Env":["NPM_CACHE=mine"],"HostConfig":{"Mounts":[{"Type":"volume","Target":"/root/.npm"}]}}`)
	_, doc := c.get()
	hc := doc["HostConfig"].(map[string]any)
	if b, ok := hc["Binds"]; ok && len(b.([]any)) != 0 {
		t.Errorf("binds %v", b)
	}
	env := doc["Env"].([]any)
	if len(env) != 1 || env[0] != "NPM_CACHE=mine" {
		t.Errorf("env %v", env)
	}
}

func TestMountSourceRedirect(t *testing.T) {
	c := &captured{}
	p := newProxy(t, startUpstream(t, echoUpstream(c)), nil)
	post(t, p, "/containers/create", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/var/run/docker.sock","Target":"/var/run/docker.sock"}]}}`)
	_, doc := c.get()
	m := doc["HostConfig"].(map[string]any)["Mounts"].([]any)[0].(map[string]any)
	if m["Source"] != p.Socket() {
		t.Errorf("source %v", m["Source"])
	}
}

func TestNetworkAndVolumeLabels(t *testing.T) {
	for _, kind := range []string{"networks", "volumes"} {
		c := &captured{}
		p := newProxy(t, startUpstream(t, echoUpstream(c)), nil)
		post(t, p, "/"+kind+"/create", `{"Name":"n"}`)
		_, doc := c.get()
		labels := doc["Labels"].(map[string]any)
		if labels[naming.MetaRunner] != "r1" || labels[naming.MetaManaged] != "1" {
			t.Errorf("%s labels %v", kind, labels)
		}
		if _, ok := doc["HostConfig"]; ok {
			t.Errorf("%s got HostConfig", kind)
		}
	}
}

func TestMalformedAndOversized(t *testing.T) {
	c := &captured{}
	p := newProxy(t, startUpstream(t, echoUpstream(c)), func(cfg *Config) { cfg.MaxBody = 64 })
	if r := post(t, p, "/containers/create", `{nope`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed status %d", r.StatusCode)
	}
	if r := post(t, p, "/containers/create", `{"Image":"`+strings.Repeat("a", 200)+`"}`); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized status %d", r.StatusCode)
	}
	if path, _ := c.get(); path != "" {
		t.Errorf("rejected request reached upstream: %s", path)
	}
}

func TestPassThroughUntouched(t *testing.T) {
	c := &captured{}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.set(r.URL.Path, b)
		w.Header().Set("X-Up", "1")
		_, _ = w.Write([]byte("pong"))
	})
	p := newProxy(t, startUpstream(t, up), nil)
	resp := post(t, p, "/build?t=x", "raw-not-json")
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "pong" || resp.Header.Get("X-Up") != "1" {
		t.Errorf("response not passed through")
	}
	if path, body := c.get(); path != "/build" || body != nil {
		t.Errorf("upstream saw path=%s", path)
	}
	c.mu.Lock()
	got := string(c.body)
	c.mu.Unlock()
	if got != "raw-not-json" {
		t.Errorf("body modified")
	}
}

func TestHijackUpgrade(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			inspectJSON(w, ownLabels("r1"))
			return
		}
		hj := w.(http.Hijacker)
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = buf.Flush()
		line, _ := buf.ReadString('\n')
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	})
	p := newProxy(t, startUpstream(t, up), nil)
	conn, err := net.Dial("unix", p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("POST /containers/abc/attach?stream=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"))
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("status %q err %v", status, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" {
			break
		}
	}
	_, _ = conn.Write([]byte("hello\n"))
	got, err := br.ReadString('\n')
	if err != nil || got != "echo:hello\n" {
		t.Fatalf("stream got %q err %v", got, err)
	}
}

func TestPullGate(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var mu sync.Mutex
	reached := 0
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached++
		mu.Unlock()
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte("{}"))
	})
	p := newProxy(t, startUpstream(t, up), nil)

	done := make(chan struct{}, 4)
	pull := func() {
		resp, err := client(p).Post("http://docker/v1.47/images/create?fromImage=x", "", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- struct{}{}
	}
	go pull()
	<-started
	if n := p.InFlightPulls(); n != 1 {
		t.Fatalf("inflight %d", n)
	}

	p.PausePulls()
	go pull()
	select {
	case <-started:
		t.Fatal("pull passed a paused gate")
	case <-time.After(200 * time.Millisecond):
	}
	if n := p.InFlightPulls(); n != 1 {
		t.Fatalf("paused pull counted as in flight: %d", n)
	}

	p.ResumePulls()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("pull not released by resume")
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("pull did not finish")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.InFlightPulls() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.InFlightPulls(); n != 0 {
		t.Fatalf("inflight %d", n)
	}
}

func TestCloseReleasesPausedPulls(t *testing.T) {
	p := newProxy(t, startUpstream(t, echoUpstream(&captured{})), nil)
	p.PausePulls()
	errc := make(chan error, 1)
	go func() {
		resp, err := client(p).Post("http://docker/images/create?fromImage=x", "", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	_ = p.Close()
	select {
	case <-errc: //nolint:errcheck // result unused; only that the client unblocks
	case <-time.After(3 * time.Second):
		t.Fatal("client stuck after Close")
	}
}

func TestDeniedEndpoints(t *testing.T) {
	hit := make(chan string, 8)
	up := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hit <- r.URL.Path })
	p := newProxy(t, startUpstream(t, up), nil)
	for _, path := range []string{
		"/swarm/init", "/v1.45/swarm/join", "/services/create", "/v1.43/services/x/update",
		"/plugins/pull", "/secrets/create", "/configs/create", "/nodes/n1", "/nodes",
	} {
		resp := post(t, p, path, "{}")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
	}
	if len(hit) != 0 {
		t.Fatal("denied request reached the upstream")
	}
	// Similar-looking but allowed paths still pass.
	if resp := post(t, p, "/containers/servicesx/start", "{}"); resp.StatusCode == http.StatusForbidden {
		t.Error("over-broad deny")
	}
}

func TestBuildLabelsCannotSetOurKeys(t *testing.T) {
	got := make(chan url.Values, 1)
	up := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got <- r.URL.Query() })
	p := newProxy(t, startUpstream(t, up), nil)
	forged, _ := json.Marshal(map[string]string{
		naming.MetaRole: "runner", strings.ToUpper(naming.MetaRunner): "x", "keep": "me",
	})
	resp := post(t, p, "/v1.45/build?t=img&labels="+url.QueryEscape(string(forged)), "tar")
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	q := <-got
	var labels map[string]string
	if err := json.Unmarshal([]byte(q.Get("labels")), &labels); err != nil {
		t.Fatal(err)
	}
	if labels[naming.MetaRole] != "job" || labels[naming.MetaRunner] != "r1" || labels["keep"] != "me" || q.Get("t") != "img" {
		t.Fatalf("%v %v", labels, q)
	}
	if _, ok := labels[strings.ToUpper(naming.MetaRunner)]; ok {
		t.Fatal("forged key kept")
	}
	// No labels param at all still gets ours; garbage is refused.
	resp = post(t, p, "/build", "tar")
	q = <-got
	if resp.StatusCode != http.StatusOK || !strings.Contains(q.Get("labels"), naming.MetaManaged) {
		t.Fatalf("%d %v", resp.StatusCode, q)
	}
	if resp := post(t, p, "/build?labels=notjson", "tar"); resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.StatusCode)
	}
}

func ownLabels(runner string) map[string]string {
	return map[string]string{
		naming.MetaManaged: "1", naming.MetaInstance: "inst", naming.MetaHostID: "h1",
		naming.MetaRunner: runner, naming.MetaRole: "job",
	}
}

func inspectJSON(w http.ResponseWriter, labels map[string]string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"Labels": labels, "Config": map[string]any{"Labels": labels}})
}

// isolationUpstream knows two containers (ours and another runner's) and
// records every non-inspect request that reaches it.
func isolationUpstream(reached chan<- string, queries chan<- url.Values) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/mine/json":
			inspectJSON(w, ownLabels("r1"))
		case r.Method == http.MethodGet && r.URL.Path == "/containers/theirs/json":
			inspectJSON(w, ownLabels("r2"))
		case r.Method == http.MethodGet && r.URL.Path == "/containers/runner/json":
			l := ownLabels("r1")
			l[naming.MetaRole] = "runner"
			inspectJSON(w, l)
		case r.Method == http.MethodGet && r.URL.Path == "/exec/x1/json":
			_, _ = w.Write([]byte(`{"ContainerID":"theirs"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/containers/ghost/json":
			http.NotFound(w, r)
		default:
			reached <- r.Method + " " + r.URL.Path
			queries <- r.URL.Query()
		}
	})
}

func do(t *testing.T, p *Proxy, method, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, "http://docker"+path, nil)
	resp, err := client(p).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestContainerIsolation(t *testing.T) {
	reached, queries := make(chan string, 16), make(chan url.Values, 16)
	p := newProxy(t, startUpstream(t, isolationUpstream(reached, queries)), nil)
	deny := []struct{ method, path string }{
		{"DELETE", "/containers/theirs?force=1"},
		{"POST", "/v1.45/containers/theirs/kill"},
		{"GET", "/containers/theirs/json"},
		{"GET", "/containers/theirs/logs"},
		{"POST", "/containers/runner/stop"}, // the runner's own container is off limits too
		{"POST", "/exec/x1/start"},
		{"POST", "/commit?container=theirs"},
	}
	for _, c := range deny {
		if got := do(t, p, c.method, c.path).StatusCode; got != http.StatusForbidden {
			t.Errorf("%s %s: status %d", c.method, c.path, got)
		}
	}
	if len(reached) != 0 {
		t.Fatal("denied request reached the daemon")
	}
	for _, c := range []struct{ method, path string }{
		{"DELETE", "/containers/mine?force=1"},
		{"POST", "/containers/mine/start"},
		{"GET", "/containers/ghost/json"},
	} {
		if got := do(t, p, c.method, c.path).StatusCode; got == http.StatusForbidden {
			t.Errorf("%s %s denied", c.method, c.path)
		}
	}
}

func TestListAndPruneAreScopedToRunner(t *testing.T) {
	reached, queries := make(chan string, 16), make(chan url.Values, 16)
	p := newProxy(t, startUpstream(t, isolationUpstream(reached, queries)), nil)
	for _, c := range []struct{ method, path string }{
		{"GET", "/containers/json?all=1&filters=" + url.QueryEscape(`{"status":["running"]}`)},
		{"GET", "/volumes"},
		{"GET", "/networks?filters=" + url.QueryEscape(`{"label":{"x=y":true}}`)},
		{"POST", "/volumes/prune"},
		{"POST", "/containers/prune"},
	} {
		do(t, p, c.method, c.path)
		<-reached
		var f map[string][]string
		if err := json.Unmarshal([]byte((<-queries).Get("filters")), &f); err != nil {
			t.Fatal(err)
		}
		have := map[string]bool{}
		for _, l := range f["label"] {
			have[l] = true
		}
		for k, v := range p.Labels() {
			if !have[k+"="+v] {
				t.Errorf("%s %s: filter lacks %s", c.method, c.path, k)
			}
		}
	}
	if resp := do(t, p, "GET", "/containers/json?filters=notjson"); resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.StatusCode)
	}
}

func TestBindAllowlist(t *testing.T) {
	reached, queries := make(chan string, 4), make(chan url.Values, 4)
	p := newProxy(t, startUpstream(t, isolationUpstream(reached, queries)), func(c *Config) {
		c.AllowedBinds = []string{"/srv/runners/r1", "/cache"}
	})
	bad := []string{
		`{"HostConfig":{"Binds":["/:/host"]}}`,
		`{"HostConfig":{"Binds":["/etc:/e:ro"]}}`,
		`{"HostConfig":{"Binds":["/srv/runners/r1/../r2:/x"]}}`,
		`{"HostConfig":{"Binds":["/srv/runners/r10:/x"]}}`,
		`{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/root","Target":"/r"}]}}`,
		`{"HostConfig":{"VolumesFrom":["theirs"]}}`,
		`{"HostConfig":{"NetworkMode":"container:theirs"}}`,
	}
	for _, b := range bad {
		if got := post(t, p, "/containers/create", b).StatusCode; got != http.StatusForbidden {
			t.Errorf("%s: status %d", b, got)
		}
	}
	good := []string{
		`{"HostConfig":{"Binds":["/srv/runners/r1/_work:/w","/cache/go:/go","namedvol:/v","/var/run/docker.sock:/var/run/docker.sock"]}}`,
		`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"/anything","Target":"/r"}],"VolumesFrom":["mine:ro"]}}`,
	}
	for _, b := range good {
		if got := post(t, p, "/containers/create", b).StatusCode; got == http.StatusForbidden {
			t.Errorf("%s denied", b)
		}
		<-reached
		<-queries
	}
	if len(reached) != 0 {
		t.Fatal("denied create reached the daemon")
	}
}
