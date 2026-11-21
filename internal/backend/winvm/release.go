package winvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// runnerVersion is the configured runner version, else the latest release
// (resolved once and cached).
func (p *Pool) runnerVersion(ctx context.Context) (string, error) {
	p.versionMu.Lock()
	defer p.versionMu.Unlock()
	if p.version != "" {
		return p.version, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.ReleaseAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: release api status %d", ErrNoRunnerVer, resp.StatusCode)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(strings.TrimSpace(rel.Tag), "v")
	if v == "" {
		return "", ErrNoRunnerVer
	}
	p.version = v
	return v, nil
}
