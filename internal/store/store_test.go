package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func TestMigrationsAndReopen(t *testing.T) {
	s, p := open(t)
	if v, _ := s.Version(); v != SchemaVersion {
		t.Fatalf("version=%d", v)
	}
	if err := s.UpsertRunner(Runner{Name: "r1", Owner: "o", OS: "linux", State: RunnerIdle, Created: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if rs, _ := s2.ListRunners(); len(rs) != 1 {
		t.Fatal("data should persist and migrations be idempotent")
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	s, p := open(t)
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(p); err == nil {
		t.Fatal("must refuse newer schema")
	}
}

func TestMeta(t *testing.T) {
	s, _ := open(t)
	if _, err := s.GetMeta(); !errors.Is(err, ErrNoMeta) {
		t.Fatalf("err=%v", err)
	}
	m := Meta{HostID: "abcd1234", Prefix: "my-ci", PreviousPrefixes: []string{"old", "older"}}
	if err := s.SetMeta(m); err != nil {
		t.Fatal(err)
	}
	m.Prefix = "new-ci"
	if err := s.SetMeta(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMeta()
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRunnerLifecycle(t *testing.T) {
	s, _ := open(t)
	base := time.Unix(100, 0)
	r := Runner{Name: "r1", Owner: "acme", Repo: "app", OS: "linux", Lane: 3, Backend: "docker", BackendID: "abc",
		GitHubRunnerID: 7, State: RunnerStarting, Created: base}
	if err := s.UpsertRunner(r); err != nil {
		t.Fatal(err)
	}
	r2 := r
	r2.Name, r2.Created = "r0", base.Add(-time.Hour)
	if err := s.UpsertRunner(r2); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.SetRunnerState("r1", RunnerRunning); !ok {
		t.Fatal("state")
	}
	if ok, _ := s.SetRunnerJob("r1", RunnerJob{Repo: "acme/app", RunID: 55, Name: "build", JobID: "jid", WorkflowRef: "acme/app/.github/workflows/ci.yml@refs/heads/main", Event: "push"}); !ok {
		t.Fatal("job")
	}
	if ok, _ := s.SetRunnerState("nope", RunnerIdle); ok {
		t.Fatal("missing runner")
	}
	got, ok, err := s.GetRunner("r1")
	if err != nil || !ok || got.State != RunnerRunning || got.JobRunID != 55 || got.JobName != "build" || got.JobID != "jid" || got.JobEvent != "push" || got.JobWorkflowRef == "" || got.Lane != 3 || !got.Created.Equal(base) {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	// A backend-id update must not wipe the job fields.
	if ok, _ := s.SetRunnerBackendID("r1", "newid"); !ok {
		t.Fatal("backend id")
	}
	if got, _, _ := s.GetRunner("r1"); got.BackendID != "newid" || got.JobID != "jid" || got.State != RunnerRunning {
		t.Fatalf("%+v", got)
	}
	if _, ok, _ := s.GetRunner("zzz"); ok {
		t.Fatal("absent")
	}
	all, _ := s.ListRunners()
	if len(all) != 2 || all[0].Name != "r0" {
		t.Fatalf("order: %+v", all)
	}
	if ok, _ := s.DeleteRunner("r1"); !ok {
		t.Fatal("delete")
	}
	if ok, _ := s.DeleteRunner("r1"); ok {
		t.Fatal("delete twice")
	}
}

func TestJobHistory(t *testing.T) {
	s, _ := open(t)
	base := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		err := s.AddJob(JobRecord{Owner: "o", Repo: "r", Workflow: "ci.yml", Job: "b", Cost: int64(i), PeakMem: 10 * int64(i),
			Duration: time.Duration(i) * time.Second, Result: "succeeded", Finished: base.Add(time.Duration(i) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddJob(JobRecord{Owner: "o", Repo: "r", Workflow: "ci.yml", Job: "other", Finished: base}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentJobs("o", "r", "ci.yml", "b", 3)
	if err != nil || len(got) != 3 || got[0].Cost != 4 || got[0].Duration != 4*time.Second {
		t.Fatalf("%+v %v", got, err)
	}
	n, err := s.PruneJobs(base.Add(2 * time.Minute))
	if err != nil || n != 3 { // i=0,1 and "other"
		t.Fatalf("pruned=%d %v", n, err)
	}
}

func TestJournalAppendOnly(t *testing.T) {
	s, _ := open(t)
	at := time.Unix(5, 0)
	if _, err := s.Append("setup", "slice", StepStarted, "", at); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.StepDone("setup", "slice"); done {
		t.Fatal("not done yet")
	}
	if _, err := s.Append("setup", "slice", StepDone, `{"ok":true}`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("migrate", "plan", StepDone, "", at); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.StepDone("setup", "slice"); !done {
		t.Fatal("done")
	}
	all, _ := s.Journal("")
	setup, _ := s.Journal("setup")
	if len(all) != 3 || len(setup) != 2 || setup[1].Status != StepDone || setup[1].Data != `{"ok":true}` || all[0].ID >= all[1].ID {
		t.Fatalf("%+v", all)
	}
}
