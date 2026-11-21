package dockerproxy

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/pulllock"
)

func TestSpecRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s", "r.json")
	in := Spec{
		Config: Config{
			Socket: "/a/dp.sock", Upstream: "/var/run/docker.sock", Runner: "r", Instance: "i", HostID: "h",
			Slice: "s.slice", OOMScoreAdj: 500, CacheBinds: []string{"/c:/c"}, CacheEnv: map[string]string{"K": "V"},
			AllowedBinds: []string{"/d"}, PersistentNames: []string{"p-*"}, PullLock: "/l", MaxBody: 5,
		},
		RunnerContainer: "r", LogLevel: "warn",
	}
	if err := WriteSpec(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.RunnerContainer != "r" || out.LogLevel != "warn" || out.Config.PullLock != "/l" || out.Config.CacheEnv["K"] != "V" ||
		out.Config.AllowedBinds[0] != "/d" || out.Config.PersistentNames[0] != "p-*" || out.Config.OOMScoreAdj != 500 || out.Config.MaxBody != 5 {
		t.Fatalf("%+v", out)
	}
	if err := WriteSpec(path, Spec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpec(path); err == nil {
		t.Fatal("an empty spec must be rejected")
	}
}

// containerUpstream is a fake daemon: /containers/<name>/json is 200 while
// *exists is true, else 404.
func containerUpstream(exists *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if exists.Load() {
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.Error(w, "no such container", http.StatusNotFound)
	})
}

func serveAsync(t *testing.T, s Spec, o ServeOptions) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, s, o) }()
	return done
}

func waitSock(t *testing.T, sock string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("proxy socket never came up")
}

func TestServeExitsWhenRunnerContainerGone(t *testing.T) {
	var exists atomic.Bool
	exists.Store(true)
	sock := filepath.Join(sockDir(t), "p.sock")
	done := serveAsync(t, Spec{
		Config:          Config{Socket: sock, Upstream: startUpstream(t, containerUpstream(&exists)), Runner: "r"},
		RunnerContainer: "r",
	}, ServeOptions{PollEvery: 20 * time.Millisecond})
	waitSock(t, sock)
	select {
	case <-done: //nolint:errcheck // only that it did not exit
		t.Fatal("proxy exited while its runner container exists")
	case <-time.After(150 * time.Millisecond):
	}
	exists.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not exit after the runner container disappeared")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket must be removed on exit: %v", err)
	}
}

// The proxy starts before the runner container is created: absence before it
// was ever seen is not an exit signal (until the grace ends).
func TestServeWaitsForRunnerContainerToAppear(t *testing.T) {
	var exists atomic.Bool
	sock := filepath.Join(sockDir(t), "p.sock")
	done := serveAsync(t, Spec{
		Config:          Config{Socket: sock, Upstream: startUpstream(t, containerUpstream(&exists)), Runner: "r"},
		RunnerContainer: "r",
	}, ServeOptions{PollEvery: 20 * time.Millisecond, StartGrace: time.Minute})
	select {
	case <-done: //nolint:errcheck // only that it did not exit
		t.Fatal("exited before the runner container was ever created")
	case <-time.After(200 * time.Millisecond):
	}
	exists.Store(true)
	time.Sleep(100 * time.Millisecond)
	exists.Store(false)
	select {
	case <-done: //nolint:errcheck // only that it exited
	case <-time.After(3 * time.Second):
		t.Fatal("no exit after the container was seen and then removed")
	}
}

func TestServeGivesUpWhenContainerNeverAppears(t *testing.T) {
	var exists atomic.Bool
	sock := filepath.Join(sockDir(t), "p.sock")
	done := serveAsync(t, Spec{
		Config:          Config{Socket: sock, Upstream: startUpstream(t, containerUpstream(&exists)), Runner: "r"},
		RunnerContainer: "r",
	}, ServeOptions{PollEvery: 20 * time.Millisecond, StartGrace: 100 * time.Millisecond})
	select {
	case <-done: //nolint:errcheck // only that it exited
	case <-time.After(3 * time.Second):
		t.Fatal("orphaned proxy never exited")
	}
}

func TestServeStopsOnContextCancel(t *testing.T) {
	var exists atomic.Bool
	exists.Store(true)
	sock := filepath.Join(sockDir(t), "p.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Spec{
			Config:          Config{Socket: sock, Upstream: startUpstream(t, containerUpstream(&exists)), Runner: "r"},
			RunnerContainer: "r",
		}, ServeOptions{PollEvery: time.Hour})
	}()
	waitSock(t, sock)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no exit on SIGTERM-style cancel")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket must be removed on exit")
	}
}

// With a pull lock configured, a proxy's pull holds the shared lock for its
// whole duration (GC, holding it exclusively, excludes pulls) and builds count.
func TestPullHoldsSharedLockAndWaitsForGC(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".pull.lock")
	lock := pulllock.New(lockPath)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte("{}"))
	})
	p := newProxy(t, startUpstream(t, up), func(c *Config) { c.PullLock = lockPath })

	for _, path := range []string{"/images/create?fromImage=x", "/build"} {
		done := make(chan struct{})
		go func() {
			resp, err := client(p).Post("http://docker"+path, "", nil)
			if err == nil {
				_ = resp.Body.Close()
			}
			close(done)
		}()
		<-started
		short, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		if ex, err := lock.Lock(short); err == nil {
			ex.Close()
			t.Fatalf("GC got the exclusive lock while %s was running", path)
		}
		cancel()
		release <- struct{}{}
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ex, err := lock.Lock(ctx)
		cancel()
		if err != nil {
			t.Fatalf("lock must be free after %s: %v", path, err)
		}

		// GC holds it: the next pull must wait, not fail, and go through on release.
		got := make(chan struct{})
		go func() {
			resp, err := client(p).Post("http://docker"+path, "", nil)
			if err == nil {
				_ = resp.Body.Close()
			}
			close(got)
		}()
		select {
		case <-started:
			t.Fatalf("%s reached the daemon while GC held the lock", path)
		case <-time.After(200 * time.Millisecond):
		}
		ex.Close()
		<-started
		release <- struct{}{}
		<-got
	}
}
