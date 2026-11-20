package migrate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
)

type cmdLog struct{ calls []string }

func (c *cmdLog) run(_ context.Context, name string, args ...string) ([]byte, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}
func (c *cmdLog) count(sub string) int {
	n := 0
	for _, x := range c.calls {
		if strings.Contains(x, sub) {
			n++
		}
	}
	return n
}

type retireEnv struct {
	root, state string
	f           *fakeGH
	cmds        *cmdLog
	r           *Retirer
	runner      LocalRunner
}

func newRetireEnv(t *testing.T) *retireEnv {
	t.Helper()
	root, state := t.TempDir(), t.TempDir()
	mk(t, root, "etc/systemd/system/actions.runner.acme-app.box-1.service", unitText)
	mk(t, root, "home/ci/actions-runner/.runner", `{"agentName":"box-1","gitHubUrl":"https://github.com/acme/app"}`)
	mk(t, root, "home/ci/actions-runner/config.sh", "#!/bin/sh")
	mk(t, root, "home/ci/actions-runner/bin/x", "bin")
	mk(t, root, "home/ci/actions-runner/_work/job/file", "keep")
	f := newFake()
	f.runners["acme/app"] = []gh.Runner{{ID: 11, Name: "box-1", Labels: []string{"self-hosted", "general"}}}
	cmds := &cmdLog{}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	r := &Retirer{Root: root, StateDir: state, Run: cmds.run, API: f, Now: func() time.Time { return now },
		Allowed: []core.Target{{Owner: "acme", Repo: "app"}}, TrustedUID: os.Getuid()}
	locals, _ := ScanLocal(root, nil, os.Getuid())
	if len(locals) != 1 {
		t.Fatalf("locals %+v", locals)
	}
	return &retireEnv{root, state, f, cmds, r, locals[0]}
}

func TestRetireArchivesAndRollbackRestores(t *testing.T) {
	e := newRetireEnv(t)
	ctx := context.Background()
	m, err := e.r.Retire(ctx, []LocalRunner{e.runner})
	if err != nil {
		t.Fatal(err)
	}
	rec := m.Runners[0]
	if rec.Status != StatusDone || !rec.Deregistered || len(e.f.deleted) != 1 || e.f.deleted[0] != 11 {
		t.Fatalf("%+v deleted=%v", rec, e.f.deleted)
	}
	if e.cmds.count("systemctl stop") != 1 || e.cmds.count("systemctl disable") != 1 || e.cmds.count("daemon-reload") != 1 {
		t.Fatalf("cmds %v", e.cmds.calls)
	}
	if exists(filepath.Join(e.root, "etc/systemd/system", e.runner.Unit)) || exists(filepath.Join(e.root, "home/ci/actions-runner/config.sh")) {
		t.Fatal("unit/dir not moved")
	}
	if !exists(filepath.Join(e.root, "home/ci/actions-runner/_work/job/file")) {
		t.Fatal("_work must stay")
	}
	for _, p := range []string{"unit/" + e.runner.Unit, "dir/config.sh", "dir/bin/x", "dir/.runner"} {
		if !exists(filepath.Join(rec.ArchiveDir, p)) {
			t.Fatalf("archive missing %s", p)
		}
	}
	if !exists(filepath.Join(m.Session, "manifest.json")) {
		t.Fatal("no manifest")
	}
	// journal covers every step
	j, _ := OpenJournal(filepath.Join(m.Session, "journal.jsonl"), nil)
	ents, _ := j.Entries()
	if len(ents) != 7 {
		t.Fatalf("journal entries %d", len(ents))
	}

	rr, err := e.r.Rollback(ctx, filepath.Join(m.Session, "manifest.json"))
	if err != nil || len(rr) != 1 {
		t.Fatal(rr, err)
	}
	if !exists(filepath.Join(e.root, "etc/systemd/system", e.runner.Unit)) || !exists(filepath.Join(e.root, "home/ci/actions-runner/bin/x")) {
		t.Fatal("files not restored")
	}
	if !rr[0].NeedsRegistration || rr[0].Token != "REGTOKEN" || rr[0].Args[0] != "./config.sh" || rr[0].Dir != "/home/ci/actions-runner" {
		t.Fatalf("%+v", rr[0])
	}
	want := map[string]bool{"--url": true, "--token": true, "--name": true, "--labels": true, "--unattended": true, "--replace": true}
	for _, a := range rr[0].Args {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing flags %v", want)
	}
	// rollback is repeatable
	if _, err := e.r.Rollback(ctx, filepath.Join(m.Session, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRetireBusyThenResume(t *testing.T) {
	e := newRetireEnv(t)
	ctx := context.Background()
	e.f.runners["acme/app"][0].Busy = true
	m, err := e.r.Retire(ctx, []LocalRunner{e.runner})
	if err != nil {
		t.Fatal(err)
	}
	if m.Runners[0].Status != StatusBusy || len(e.cmds.calls) != 0 || !exists(filepath.Join(e.root, "home/ci/actions-runner/config.sh")) {
		t.Fatalf("busy runner must be untouched: %+v %v", m.Runners[0], e.cmds.calls)
	}
	e.f.runners["acme/app"][0].Busy = false
	e.f.busyDel[11] = true // GitHub still says busy on delete
	m, err = e.r.Retire(ctx, []LocalRunner{e.runner})
	if err != nil {
		t.Fatal(err)
	}
	if m.Runners[0].Status != StatusDeregister || m.Runners[0].Deregistered {
		t.Fatalf("%+v", m.Runners[0])
	}
	if e.cmds.count("systemctl stop") != 0 {
		t.Fatal("must not stop before deregistering")
	}
	e.f.busyDel[11] = false
	m2, err := e.r.Retire(ctx, []LocalRunner{e.runner})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Session != m.Session || m2.Runners[0].Status != StatusDone || len(e.f.deleted) != 1 {
		t.Fatalf("%+v deleted=%v", m2.Runners[0], e.f.deleted)
	}
	if e.cmds.count("systemctl stop") != 1 {
		t.Fatal("stop must run exactly once")
	}
}

type errAPI struct{ RunnerAPI }

func (errAPI) ListRunners(context.Context, core.Target) ([]gh.Runner, error) {
	return nil, errors.New("api down")
}

func TestRetireFailsClosedWhenGitHubUnreachable(t *testing.T) {
	e := newRetireEnv(t)
	e.r.API = errAPI{e.f}
	m, err := e.r.Retire(context.Background(), []LocalRunner{e.runner})
	if err != nil {
		t.Fatal(err)
	}
	if m.Runners[0].Status != StatusFailed || len(e.cmds.calls) != 0 {
		t.Fatalf("%+v %v", m.Runners[0], e.cmds.calls)
	}
}

func TestRenameAtRefusesOverwrite(t *testing.T) {
	d := t.TempDir()
	mk(t, d, "a", "1")
	mk(t, d, "b", "2")
	fd, err := openPlainDir(d)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFd(fd)
	if err := renameAt(fd, "a", fd, "b"); !errors.Is(err, fs.ErrExist) {
		t.Fatal("want ErrExist, got", err)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "b")); string(b) != "2" {
		t.Fatal("destination clobbered")
	}
}

func (e *retireEnv) retire(t *testing.T) *Manifest {
	t.Helper()
	m, err := e.r.Retire(context.Background(), []LocalRunner{e.runner})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRetireRefusesSymlinkedRunnerDir(t *testing.T) {
	e := newRetireEnv(t)
	victim := filepath.Join(e.root, "etc/victim")
	mk(t, e.root, "etc/victim/passwd", "x")
	dir := filepath.Join(e.root, "home/ci/actions-runner")
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatal(err)
	}
	m := e.retire(t)
	if m.Runners[0].Status != StatusFailed || !exists(filepath.Join(victim, "passwd")) {
		t.Fatalf("%+v", m.Runners[0])
	}
}

func TestRetireRefusesSwappedInode(t *testing.T) {
	e := newRetireEnv(t)
	dir := filepath.Join(e.root, "home/ci/actions-runner")
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	mk(t, e.root, "home/ci/actions-runner/config.sh", "new")
	m := e.retire(t)
	if m.Runners[0].Status != StatusFailed || !exists(filepath.Join(dir, "config.sh")) {
		t.Fatalf("%+v", m.Runners[0])
	}
}

func TestRetireRefusesSymlinkedParent(t *testing.T) {
	e := newRetireEnv(t)
	home := filepath.Join(e.root, "home/ci")
	mk(t, e.root, "etc/victim/actions-runner/keep", "x")
	if err := os.Rename(home, home+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.root, "etc/victim"), home); err != nil {
		t.Fatal(err)
	}
	m := e.retire(t)
	if m.Runners[0].Status != StatusFailed || !exists(filepath.Join(e.root, "etc/victim/actions-runner/keep")) {
		t.Fatalf("%+v", m.Runners[0])
	}
}

func TestRetireRefusesNonTargetAndIneligible(t *testing.T) {
	e := newRetireEnv(t)
	e.r.Allowed = []core.Target{{Owner: "other"}}
	m := e.retire(t)
	if len(m.Runners) != 0 || len(m.Refused) != 1 || m.Refused[0].Reason != ReasonNotAllowed || len(e.cmds.calls) != 0 {
		t.Fatalf("%+v", m)
	}
	e.r.Allowed = []core.Target{{Owner: "acme", Repo: "app"}}
	lr := e.runner
	lr.Eligible = false
	m, _ = e.r.Retire(context.Background(), []LocalRunner{lr})
	if len(m.Runners) != 0 || m.Refused[0].Reason != ReasonNotEligible {
		t.Fatalf("%+v", m)
	}
	e.r.Allowed = nil
	if m = e.retire(t); len(m.Runners) != 0 {
		t.Fatal("empty allow-list must refuse everything")
	}
}

func TestScanOnlyUnitRunnersAreEligible(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "home/mallory/r/.runner", `{"agentName":"prod-1","gitHubUrl":"https://github.com/acme/app"}`)
	got, _ := ScanLocal(root, nil, os.Getuid())
	if len(got) != 1 || got[0].Eligible {
		t.Fatalf("%+v", got)
	}
	// unit whose name does not match the .runner file
	mk(t, root, "etc/systemd/system/actions.runner.acme-app.box-1.service", "[Service]\nWorkingDirectory=/home/mallory/r\n")
	got, _ = ScanLocal(root, nil, os.Getuid())
	for _, l := range got {
		if l.Eligible {
			t.Fatalf("mismatched name must not be eligible: %+v", l)
		}
	}
}

func TestScanUnitFromWrongOwnerNotEligible(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "etc/systemd/system/actions.runner.acme-app.box-1.service", unitText)
	mk(t, root, "home/ci/actions-runner/.runner", `{"agentName":"box-1","gitHubUrl":"https://github.com/acme/app"}`)
	got, _ := ScanLocal(root, nil, os.Getuid()+1)
	for _, l := range got {
		if l.Eligible {
			t.Fatalf("%+v", l)
		}
	}
}

func TestRollbackRefusesSymlinkedDestination(t *testing.T) {
	e := newRetireEnv(t)
	m := e.retire(t)
	dir := filepath.Join(e.root, "home/ci/actions-runner")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(e.root, "etc/profile.d")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatal(err)
	}
	_, err := e.r.Rollback(context.Background(), filepath.Join(m.Session, "manifest.json"))
	if err == nil {
		t.Fatal("want refusal")
	}
	if ents, _ := os.ReadDir(victim); len(ents) != 0 {
		t.Fatalf("files escaped into symlink target: %v", ents)
	}
}

func TestRollbackRefusesSwappedInode(t *testing.T) {
	e := newRetireEnv(t)
	m := e.retire(t)
	dir := filepath.Join(e.root, "home/ci/actions-runner")
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Rollback(context.Background(), filepath.Join(m.Session, "manifest.json")); err == nil {
		t.Fatal("want refusal")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatal("restored into a different directory")
	}
}

func TestIdleCheckRepeatsUntilDeregistered(t *testing.T) {
	e := newRetireEnv(t)
	e.f.busyDel[11] = true
	e.retire(t)
	e.f.runners["acme/app"][0].Busy = true
	m := e.retire(t)
	if m.Runners[0].Status != StatusBusy || e.cmds.count("systemctl stop") != 0 {
		t.Fatalf("%+v %v", m.Runners[0], e.cmds.calls)
	}
}

func TestValidIdent(t *testing.T) {
	for s, want := range map[string]bool{"box-1": true, "a.b_c": true, "": false, "a b": false, "$(x)": false, "a\"b": false, "x;y": false} {
		if ValidIdent(s) != want {
			t.Errorf("%q", s)
		}
	}
}
