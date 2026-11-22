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
	if !strings.Contains(startJobScript(j, "Interactive", false), tc) {
		t.Fatal("tool cache dir is not created before the job")
	}
	if strings.Contains(cleanupScript(j, true), tc) {
		t.Fatal("cleanup must not touch the tool cache")
	}
}

func TestGitBashBeforeSystem32(t *testing.T) {
	cmd := jobPaths{Runner: "r", RunnerDir: `C:\actions-runner`, WorkRoot: `C:\rw`}.taskCmd()
	path := strings.Index(cmd, `set "PATH=%ProgramFiles%\Git\bin;%PATH%"`)
	if path < 0 || path > strings.Index(cmd, "run.cmd") {
		t.Fatal("Git's bin must lead PATH before the runner starts")
	}
}
