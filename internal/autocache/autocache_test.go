package autocache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
)

func TestScope(t *testing.T) {
	j := core.Job{Owner: "Org", Repo: "App"}
	if s := Scope("repo", core.Target{Owner: "org"}, j); s != "org/app" {
		t.Fatal(s)
	}
	if s := Scope("org", core.Target{Owner: "org"}, j); s != "org" {
		t.Fatal(s)
	}
	if s := Scope("repo", core.Target{Owner: "me", Repo: "tool"}, core.Job{}); s != "me/tool" {
		t.Fatal("no job yet: the lane's repo", s)
	}
	if s := Scope("repo", core.Target{Owner: "org"}, core.Job{}); s != "org" {
		t.Fatal("an org lane with no job known yet uses the org", s)
	}
}

func TestEnvForKeepsConfiguredCaches(t *testing.T) {
	e := EnvFor(map[string]bool{"PUB_CACHE": true})
	if _, ok := e["PUB_CACHE"]; ok || e["NUGET_PACKAGES"] != MountPath+"/nuget" || e["RUNNER_TOOL_CACHE"] != MountPath+"/tool" {
		t.Fatalf("%v", e)
	}
}

func TestScanAndEvictLeastRecentlyUsed(t *testing.T) {
	base := t.TempDir()
	write := func(scope string, n int, used time.Time) {
		if err := Touch(base, scope, os.Getuid()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(Dir(base, scope), "blob"), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(Dir(base, scope), usedFile), used, used); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("o/new", 100, now)
	write("o/mid", 100, now.Add(-time.Hour))
	write("o/old", 100, now.Add(-2*time.Hour))
	write("o/busy", 100, now.Add(-3*time.Hour))
	us, err := Scan(base)
	if err != nil || len(us) != 4 || us[0].Scope != "o/new" || us[3].Scope != "o/busy" || us[0].Bytes != 100 {
		t.Fatalf("%+v %v", us, err)
	}
	gone, err := Evict(base, us, 250, map[string]bool{"o/busy": true})
	if err != nil || len(gone) != 2 || gone[0].Scope != "o/old" || gone[1].Scope != "o/mid" {
		t.Fatalf("in-use caches stay, oldest go first: %+v %v", gone, err)
	}
	if _, err := os.Stat(Dir(base, "o/busy")); err != nil {
		t.Fatal("busy cache removed")
	}
	if us, _ := Scan(base); len(us) != 2 {
		t.Fatalf("%+v", us)
	}
}
