package winvm

import (
	"strings"
	"testing"
)

func TestToolCacheOutlivesTheJob(t *testing.T) {
	j := jobPaths{Runner: "home-ci-h-1", RunnerDir: `C:\actions-runner`, WorkRoot: `C:\rw`}
	tc := j.ToolCache()
	if strings.HasPrefix(strings.ToLower(tc), strings.ToLower(j.Dir())) {
		t.Fatalf("tool cache %s is inside the per-job dir %s, which cleanup deletes", tc, j.Dir())
	}
	if !strings.Contains(j.taskCmd(), `set "RUNNER_TOOL_CACHE=`+tc+`"`) {
		t.Fatal("runner does not get the persistent tool cache")
	}
	if !strings.Contains(startJobScript(j, "Interactive"), tc) {
		t.Fatal("tool cache dir is not created before the job")
	}
	if strings.Contains(cleanupScript(j), tc) {
		t.Fatal("cleanup must not touch the tool cache")
	}
}
