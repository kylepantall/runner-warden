// Command m0 is the M0 spike: a throwaway scale-set listener that starts one
// ephemeral runner container per job and logs everything needed to answer the
// M0 questions (auth, message contents, hook ordering, sibling Docker jobs,
// name limits). Not production code.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/google/uuid"
)

const ownerLabel = "io.github.kylepantall.runner-warden.spike"

type opts struct {
	url, name, image, hostDir, socket, skip, slice string
	labels                                         []string
	max, sockGID, capacity                         int
	proxy, noRunners, manualAcquire                bool
	control                                        string
}

func main() {
	var o opts
	var labels string
	mode := flag.String("mode", "listen", "listen | names")
	flag.StringVar(&o.url, "url", "", "registration URL (https://github.com/owner[/repo])")
	flag.StringVar(&o.name, "name", "", "scale set name")
	flag.StringVar(&labels, "labels", "", "extra labels, comma-separated")
	flag.StringVar(&o.image, "image", "ghcr.io/actions/actions-runner:latest", "runner image")
	flag.StringVar(&o.hostDir, "host-dir", "/var/lib/rw-spike", "host dir mounted at the same path in runners")
	flag.StringVar(&o.socket, "socket", "/var/run/docker.sock", "docker socket mounted into runners")
	flag.IntVar(&o.sockGID, "socket-gid", 0, "gid owning the docker socket")
	flag.StringVar(&o.skip, "skip", "", "regex on job display name: never acquire matching jobs")
	flag.IntVar(&o.max, "max", 4, "max runners")
	flag.BoolVar(&o.proxy, "proxy", false, "give each runner its own Docker socket proxy")
	flag.IntVar(&o.capacity, "capacity", -1, "maxCapacity sent to the service (default: -max)")
	flag.BoolVar(&o.noRunners, "no-runners", false, "never start runners automatically")
	flag.BoolVar(&o.manualAcquire, "manual-acquire", false, "never acquire automatically; use the control socket")
	flag.StringVar(&o.control, "control", "", "unix socket for the control API")
	flag.StringVar(&o.slice, "slice", "rw-spike.slice", "cgroup parent for job containers (proxy mode)")
	flag.Parse()
	if labels != "" {
		o.labels = strings.Split(labels, ",")
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     o.url,
		PersonalAccessToken: os.Getenv("RW_TOKEN"),
		SystemInfo:          scaleset.SystemInfo{System: "runner-warden-spike", Version: "m0"},
	})
	if err != nil {
		fail(log, "client", err)
	}

	switch *mode {
	case "names":
		err = nameLimits(ctx, log, client, o)
	case "check":
		var ss *scaleset.RunnerScaleSet
		ss, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: o.name, RunnerGroupID: 1, Labels: scaleSetLabels(o),
		})
		log.Info("create scale set", "url", o.url, "ok", err == nil, "error", errStr(err))
		if err == nil {
			err = client.DeleteRunnerScaleSet(ctx, ss.ID)
			log.Info("delete scale set", "ok", err == nil, "error", errStr(err))
		}
	default:
		err = listen(ctx, log, client, o)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fail(log, "run", err)
	}
}

func fail(log *slog.Logger, what string, err error) {
	log.Error("spike failed", "step", what, "error", err.Error())
	os.Exit(1)
}

func scaleSetLabels(o opts) []scaleset.Label {
	l := []scaleset.Label{{Name: o.name}}
	for _, x := range o.labels {
		l = append(l, scaleset.Label{Name: strings.TrimSpace(x)})
	}
	return l
}

func listen(ctx context.Context, log *slog.Logger, client *scaleset.Client, o opts) error {
	ss, err := client.GetRunnerScaleSet(ctx, 1, o.name)
	if err != nil || ss == nil {
		ss, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: o.name, RunnerGroupID: 1, Labels: scaleSetLabels(o),
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("create scale set: %w", err)
		}
	}
	log.Info("scale set ready", "id", ss.ID, "labels", ss.Labels)
	defer func() {
		if err := client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), ss.ID); err != nil {
			log.Error("delete scale set failed", "error", err.Error())
		}
	}()

	dc, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	pull, err := dc.ImagePull(ctx, o.image, image.PullOptions{})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, pull)
	_ = pull.Close()

	host, _ := os.Hostname()
	sess, err := client.MessageSessionClient(ctx, ss.ID, host)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer sess.Close(context.Background())

	capacity := o.max
	if o.capacity >= 0 {
		capacity = o.capacity
	}
	l, err := listener.New(sess, listener.Config{ScaleSetID: ss.ID, MaxRunners: capacity, Logger: log.WithGroup("listener")})
	if err != nil {
		return err
	}
	s := &spikeScaler{log: log, o: o, dc: dc, client: client, sess: sess, ssID: ss.ID,
		runners: map[string]string{}, proxies: map[string]*dockerProxy{}, available: map[int64]*scaleset.JobAvailable{}}
	if o.skip != "" {
		s.skip = regexp.MustCompile(o.skip)
	}
	if o.control != "" {
		if err := serveControl(ctx, o.control, l, s); err != nil {
			return fmt.Errorf("control: %w", err)
		}
	}
	defer s.shutdown(context.WithoutCancel(ctx))
	return l.Run(ctx, s)
}

type spikeScaler struct {
	log    *slog.Logger
	o      opts
	dc     *dockerclient.Client
	client *scaleset.Client
	sess   listener.Client
	ssID   int
	skip   *regexp.Regexp

	mu        sync.Mutex
	runners   map[string]string // runner name -> container id
	proxies   map[string]*dockerProxy
	available map[int64]*scaleset.JobAvailable // announced, not yet acquired
}

func dump(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *spikeScaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		return nil
	}
	s.log.Debug("raw message", "id", msg.MessageID, "available", len(msg.JobAvailableMessages),
		"assigned", len(msg.JobAssignedMessages), "started", len(msg.JobStartedMessages), "completed", len(msg.JobCompletedMessages))
	for _, j := range msg.JobAssignedMessages {
		s.log.Info("job assigned", "job", dump(j))
	}
	var acquire []int64
	for _, j := range msg.JobAvailableMessages {
		skipped := s.o.manualAcquire || (s.skip != nil && s.skip.MatchString(j.JobDisplayName))
		s.log.Info("job available", "skipped", skipped, "job", dump(j))
		if skipped {
			s.mu.Lock()
			s.available[j.RunnerRequestID] = j
			s.mu.Unlock()
		} else {
			acquire = append(acquire, j.RunnerRequestID)
		}
	}
	if len(acquire) > 0 {
		got, err := s.sess.AcquireJobs(ctx, acquire)
		s.log.Info("acquire", "requested", acquire, "acquired", got, "error", errStr(err))
		if err != nil {
			return err
		}
	}
	for _, j := range msg.JobStartedMessages {
		s.log.Info("job started", "job", dump(j))
	}
	for _, j := range msg.JobCompletedMessages {
		s.log.Info("job completed", "job", dump(j))
		s.remove(ctx, j.RunnerName)
	}
	if msg.Statistics == nil {
		return nil
	}
	s.log.Info("statistics", "stats", dump(msg.Statistics))
	if s.o.noRunners {
		return nil
	}
	want := min(s.o.max, msg.Statistics.TotalAssignedJobs)
	for s.count() < want {
		if err := s.start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *spikeScaler) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.runners) }

func (s *spikeScaler) start(ctx context.Context) error {
	name := fmt.Sprintf("%s-%s", s.o.name, uuid.NewString()[:8])
	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.ssID)
	if err != nil {
		return fmt.Errorf("jit: %w", err)
	}
	// The runner lives at the same path on host and in the container, so
	// sibling job containers (container:/services:) can bind-mount its
	// workspace and externals.
	dir := filepath.Join(s.o.hostDir, "runners", name)
	hooks := filepath.Join(s.o.hostDir, "hooks")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o777)
	cmd := fmt.Sprintf("cp -RP /home/runner/. %s/ && cd %s && exec ./run.sh", dir, dir)
	sock := s.o.socket
	if s.o.proxy {
		sock = dir + ".sock"
		p, err := startProxy(s.log, sock, s.o.socket, name, s.o.slice)
		if err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
		s.mu.Lock()
		s.proxies[name] = p
		s.mu.Unlock()
	}
	c, err := s.dc.ContainerCreate(ctx,
		&container.Config{
			Image:  s.o.image,
			User:   "runner",
			Cmd:    []string{"bash", "-c", cmd},
			Labels: map[string]string{ownerLabel: "1", ownerLabel + ".runner": name},
			Env: []string{
				"ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit.EncodedJITConfig,
				"ACTIONS_RUNNER_HOOK_JOB_STARTED=" + filepath.Join(hooks, "started.sh"),
				"ACTIONS_RUNNER_HOOK_JOB_COMPLETED=" + filepath.Join(hooks, "completed.sh"),
				"RW_HOOK_LOG=" + filepath.Join(s.o.hostDir, "hook.log"),
			},
		},
		&container.HostConfig{
			Binds: []string{
				sock + ":/var/run/docker.sock",
				dir + ":" + dir,
				hooks + ":" + hooks + ":ro",
				s.o.hostDir + "/hook.log:" + s.o.hostDir + "/hook.log",
			},
			GroupAdd: []string{fmt.Sprint(s.o.sockGID)},
		},
		nil, nil, name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := s.dc.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	s.mu.Lock()
	s.runners[name] = c.ID
	s.mu.Unlock()
	s.log.Info("runner started", "name", name, "container", c.ID[:12])
	return nil
}

func (s *spikeScaler) remove(ctx context.Context, name string) {
	s.mu.Lock()
	id, ok := s.runners[name]
	delete(s.runners, name)
	p := s.proxies[name]
	delete(s.proxies, name)
	s.mu.Unlock()
	if p != nil {
		p.close()
		cleanupRunner(ctx, s.log, s.dc, name)
	}
	if !ok {
		s.log.Warn("completed job for unknown runner", "name", name)
		return
	}
	err := s.dc.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
	s.log.Info("runner removed", "name", name, "error", errStr(err))
}

func (s *spikeScaler) shutdown(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, id := range s.runners {
		err := s.dc.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
		s.log.Info("shutdown: runner removed", "name", name, "error", errStr(err))
	}
}

// nameLimits probes GitHub's limits for scale set names and runner names.
func nameLimits(ctx context.Context, log *slog.Logger, client *scaleset.Client, o opts) error {
	for _, n := range []int{40, 64, 65, 100} {
		name := o.name + "-" + strings.Repeat("x", max(0, n-len(o.name)-1))
		ss, err := client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name: name, RunnerGroupID: 1, Labels: []scaleset.Label{{Name: name}},
		})
		log.Info("scale set name", "len", len(name), "ok", err == nil, "error", errStr(err))
		if err != nil {
			continue
		}
		for _, rn := range []int{50, 64, 65, 100} {
			runner := "r-" + strings.Repeat("y", rn-2)
			jit, err := client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: runner}, ss.ID)
			log.Info("runner name", "scaleSetLen", len(name), "len", len(runner), "ok", err == nil, "error", errStr(err))
			if err == nil && jit.Runner != nil {
				_ = client.RemoveRunner(ctx, int64(jit.Runner.ID))
			}
		}
		_ = client.DeleteRunnerScaleSet(ctx, ss.ID)
	}
	return nil
}
