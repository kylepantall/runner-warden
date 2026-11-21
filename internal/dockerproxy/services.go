package dockerproxy

import "strings"

// Service containers of jobs that do not run in a container.
//
// On a GitHub-hosted (or bare-host) runner, a job's `services:` publish
// ports on the host and steps reach them on localhost. Here the job's steps
// run inside the runner container, whose localhost is its own network
// namespace, and a fixed host port (`5432:5432`) also collides between jobs
// running side by side. So for such jobs the proxy starts each service in
// the runner container's network namespace instead: the service listens on
// the runner's localhost, exactly what the steps expect, and nothing is
// published on the host.
//
// The runner creates all of a job's containers on one `github_network_*`
// network, the job container (when the job has `container:`) first. A job
// container mounts the workspace at /__w; when one was created on the
// network, the job talks to its services by name there and nothing changes.

const jobNetworkPrefix = "github_network_"

// jobWorkspace is where the runner mounts the workspace in a job container.
const jobWorkspace = "/__w"

// noteJobContainer records a job container's network; it reports whether doc
// is one (on a job network, with the workspace mounted).
func (p *Proxy) noteJobContainer(doc map[string]any) bool {
	hc, _ := doc["HostConfig"].(map[string]any)
	nm, _ := hc["NetworkMode"].(string)
	if !strings.HasPrefix(nm, jobNetworkPrefix) {
		return false
	}
	binds, _ := hc["Binds"].([]any)
	mounts, _ := hc["Mounts"].([]any)
	if !usedTargets(binds, mounts, doc["Volumes"])[jobWorkspace] {
		return false
	}
	p.mu.Lock()
	if p.containerJobNets == nil {
		p.containerJobNets = map[string]bool{}
	}
	p.containerJobNets[nm] = true
	p.mu.Unlock()
	return true
}

// shareRunnerNetwork moves a service container of a job without a job
// container into the runner container's network namespace. It reports
// whether it did.
func (p *Proxy) shareRunnerNetwork(doc map[string]any) bool {
	if p.cfg.Runner == "" {
		return false
	}
	hc, _ := doc["HostConfig"].(map[string]any)
	nm, _ := hc["NetworkMode"].(string)
	if !strings.HasPrefix(nm, jobNetworkPrefix) {
		return false
	}
	p.mu.Lock()
	containerJob := p.containerJobNets[nm]
	p.mu.Unlock()
	if containerJob {
		return false
	}
	hc["NetworkMode"] = "container:" + p.cfg.Runner
	// Docker refuses these together with a container network mode.
	for _, k := range []string{"PortBindings", "PublishAllPorts", "Links", "Dns", "DnsOptions", "DnsSearch", "ExtraHosts"} {
		delete(hc, k)
	}
	for _, k := range []string{"ExposedPorts", "Hostname", "Domainname", "MacAddress", "NetworkingConfig"} {
		delete(doc, k)
	}
	return true
}
