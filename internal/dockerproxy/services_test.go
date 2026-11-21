package dockerproxy

import (
	"net/http"
	"testing"
)

const svcBody = `{"Image":"postgres:16","ExposedPorts":{"5432/tcp":{}},"HostConfig":{"NetworkMode":"github_network_abc","PortBindings":{"5432/tcp":[{"HostPort":"5432"}]}},"NetworkingConfig":{"EndpointsConfig":{"github_network_abc":{"Aliases":["postgres"]}}}}`

func TestServiceOfNonContainerJobSharesRunnerNetwork(t *testing.T) {
	c := &captured{}
	p := newProxy(t, startUpstream(t, echoUpstream(c)), nil)
	if resp := post(t, p, "/v1.47/containers/create?name=svc", svcBody); resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, doc := c.get()
	hc := doc["HostConfig"].(map[string]any)
	if hc["NetworkMode"] != "container:r1" {
		t.Fatalf("service must join the runner's network namespace: %v", hc["NetworkMode"])
	}
	for _, k := range []string{"PortBindings"} {
		if _, ok := hc[k]; ok {
			t.Errorf("HostConfig.%s must be dropped", k)
		}
	}
	for _, k := range []string{"ExposedPorts", "NetworkingConfig"} {
		if _, ok := doc[k]; ok {
			t.Errorf("%s must be dropped", k)
		}
	}
}

func TestServiceOfContainerJobKeepsJobNetwork(t *testing.T) {
	c := &captured{}
	p := newProxy(t, startUpstream(t, echoUpstream(c)), func(cfg *Config) { cfg.AllowedBinds = []string{"/work"} })
	job := `{"Image":"node:22","HostConfig":{"NetworkMode":"github_network_abc","Binds":["/work/_work:/__w"]}}`
	post(t, p, "/containers/create?name=job", job)
	post(t, p, "/containers/create?name=svc", svcBody)
	_, doc := c.get()
	hc := doc["HostConfig"].(map[string]any)
	if hc["NetworkMode"] != "github_network_abc" || hc["PortBindings"] == nil || doc["NetworkingConfig"] == nil {
		t.Fatalf("a container job's service must stay on the job network: %v", hc)
	}
}

func TestOtherContainersKeepTheirNetwork(t *testing.T) {
	c := &captured{}
	p := newProxy(t, startUpstream(t, echoUpstream(c)), nil)
	post(t, p, "/containers/create", `{"Image":"nginx","HostConfig":{"NetworkMode":"bridge","PortBindings":{"80/tcp":[{"HostPort":"8080"}]}}}`)
	_, doc := c.get()
	if hc := doc["HostConfig"].(map[string]any); hc["NetworkMode"] != "bridge" || hc["PortBindings"] == nil {
		t.Fatalf("containers outside job networks are untouched: %v", hc)
	}
}
