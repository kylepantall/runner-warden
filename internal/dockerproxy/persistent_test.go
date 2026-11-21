package dockerproxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kylepantall/runner-warden/internal/naming"
)

func persistentLabelSet(instance string) map[string]string {
	return map[string]string{
		naming.MetaManaged: "1", naming.MetaInstance: instance, naming.MetaHostID: "h1",
		naming.MetaRole: rolePersistent,
	}
}

func withPersistent(c *Config) {
	c.PersistentNames = []string{"dev-*"}
	c.AllowedBinds = []string{"/w"}
}

func TestPersistentCreateLabels(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		persistent bool
	}{
		{"matching", "?name=dev-5", true},
		{"non matching", "?name=other", false},
		{"no name", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &captured{}
			p := newProxy(t, startUpstream(t, echoUpstream(c)), withPersistent)
			// The client tries to pin the container to another runner.
			body := `{"Image":"a","Labels":{"` + naming.MetaRunner + `":"evil"},"HostConfig":{"Binds":["/run/docker.sock:/s","/w:/w"]}}`
			if got := post(t, p, "/containers/create"+tc.query, body).StatusCode; got != http.StatusCreated {
				t.Fatalf("status %d", got)
			}
			_, doc := c.get()
			labels := doc["Labels"].(map[string]any)
			hc := doc["HostConfig"].(map[string]any)
			if tc.persistent {
				for k, v := range persistentLabelSet("inst") {
					if labels[k] != v {
						t.Errorf("label %s = %v", k, labels[k])
					}
				}
				if r, ok := labels[naming.MetaRunner]; ok && r != "" {
					t.Errorf("runner label present: %v", r)
				}
			} else {
				if labels[naming.MetaRole] != roleJob || labels[naming.MetaRunner] != "r1" {
					t.Errorf("not a job container: %v", labels)
				}
			}
			// Slice, oom score, socket redirect and bind allowlist apply to both.
			if hc["CgroupParent"] != "rw.slice" || hc["OomScoreAdj"] != float64(500) {
				t.Errorf("host config not rewritten: %v", hc)
			}
			if binds := hc["Binds"].([]any); binds[0] != p.Socket()+":/s" {
				t.Errorf("socket not redirected: %v", binds)
			}
			bad := `{"Image":"a","HostConfig":{"Binds":["/etc:/x"]}}`
			if got := post(t, p, "/containers/create"+tc.query, bad).StatusCode; got != http.StatusForbidden {
				t.Errorf("bind allowlist not enforced: %d", got)
			}
		})
	}
}

func persistentUpstream(reached chan<- string, queries chan<- url.Values, list string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/containers/dev-5/json":
			inspectJSON(w, persistentLabelSet("inst"))
		case r.Method == http.MethodGet && r.URL.Path == "/containers/foreign/json":
			inspectJSON(w, persistentLabelSet("other-inst"))
		case r.Method == http.MethodGet && r.URL.Path == "/containers/theirs/json":
			inspectJSON(w, ownLabels("r2"))
		case r.Method == http.MethodGet && r.URL.Path == "/exec/pe/json":
			_, _ = w.Write([]byte(`{"ContainerID":"dev-5"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/exec/te/json":
			_, _ = w.Write([]byte(`{"ContainerID":"theirs"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			queries <- r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(list))
		default:
			reached <- r.Method + " " + r.URL.Path
			queries <- r.URL.Query()
		}
	})
}

func TestPersistentAccessAcrossRunners(t *testing.T) {
	reached, queries := make(chan string, 32), make(chan url.Values, 32)
	// This proxy is another runner: it did not create dev-5.
	p := newProxy(t, startUpstream(t, persistentUpstream(reached, queries, "[]")), func(c *Config) { c.Runner = "r9" })
	allow := []struct{ method, path string }{
		{"GET", "/containers/dev-5/json"},
		{"POST", "/containers/dev-5/exec"},
		{"GET", "/containers/dev-5/logs"},
		{"GET", "/containers/dev-5/archive?path=/x"},
		{"DELETE", "/containers/dev-5?force=1"},
		{"POST", "/exec/pe/start"},
		{"POST", "/exec/pe/resize"},
	}
	for _, c := range allow {
		if got := do(t, p, c.method, c.path).StatusCode; got == http.StatusForbidden {
			t.Errorf("%s %s denied", c.method, c.path)
		}
	}
	deny := []struct{ method, path string }{
		{"POST", "/containers/theirs/exec"},
		{"DELETE", "/containers/theirs?force=1"},
		{"POST", "/exec/te/start"},
		{"POST", "/containers/foreign/kill"}, // persistent, but another instance
	}
	for _, c := range deny {
		if got := do(t, p, c.method, c.path).StatusCode; got != http.StatusForbidden {
			t.Errorf("%s %s: status %d", c.method, c.path, got)
		}
	}
}

func TestPersistentOwnershipDoesNotCoverNetworks(t *testing.T) {
	p := newProxy(t, startUpstream(t, http.NotFoundHandler()), nil)
	if p.ownsLabels("networks", persistentLabelSet("inst")) {
		t.Fatal("persistent role must only apply to containers")
	}
}

func TestContainerListPostFilter(t *testing.T) {
	summary := func(id string, labels map[string]string) map[string]any {
		return map[string]any{"Id": id, "Labels": labels, "Extra": id}
	}
	items := []map[string]any{
		summary("mine", ownLabels("r1")),
		summary("theirs", ownLabels("r2")),
		summary("pers", persistentLabelSet("inst")),
		summary("foreign", persistentLabelSet("other-inst")),
		summary("plain", nil),
	}
	raw, _ := json.Marshal(items)
	reached, queries := make(chan string, 8), make(chan url.Values, 8)
	p := newProxy(t, startUpstream(t, persistentUpstream(reached, queries, string(raw))), nil)

	ids := func(path string) []string {
		resp := do(t, p, "GET", path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var got []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got))
		for _, g := range got {
			out = append(out, g["Id"].(string))
		}
		return out
	}
	if got := strings.Join(ids("/v1.45/containers/json?all=1"), ","); got != "mine,pers" {
		t.Fatalf("got %s", got)
	}
	q := <-queries
	var f map[string][]string
	if err := json.Unmarshal([]byte(q.Get("filters")), &f); err != nil {
		t.Fatal(err)
	}
	have := strings.Join(f["label"], ",")
	if !strings.Contains(have, naming.MetaInstance+"=inst") || strings.Contains(have, naming.MetaRunner) {
		t.Fatalf("upstream filter %v", f)
	}
	// limit is applied after filtering, not upstream.
	if got := ids("/containers/json?limit=1"); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("limit: %v", got)
	}
	if q := <-queries; q.Has("limit") {
		t.Fatal("limit forwarded upstream")
	}
}
