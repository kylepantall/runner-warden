package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"

	"github.com/actions/scaleset/listener"
	"github.com/docker/docker/api/types/container"
)

// serveControl exposes a tiny HTTP API on a unix socket so experiments can
// drive the listener by hand:
//
//	GET  /state               available (not acquired) jobs and runners
//	POST /capacity?n=N        maxCapacity sent on the next poll
//	POST /acquire?tag=T       acquire available jobs whose display name is T
//	POST /start               start one runner
//	POST /kill?name=R         force-remove a runner container
func serveControl(ctx context.Context, path string, l *listener.Listener, s *spikeScaler) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var avail []string
		for _, j := range s.available {
			avail = append(avail, fmt.Sprintf("%d %s queued=%s", j.RunnerRequestID, j.JobDisplayName, j.QueueTime.Format("15:04:05")))
		}
		sort.Strings(avail)
		names := make([]string, 0, len(s.runners))
		for n := range s.runners {
			names = append(names, n)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"available": avail, "runners": names})
	})
	mux.HandleFunc("POST /capacity", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 0 {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		l.SetMaxRunners(n)
		s.log.Info("control: capacity set", "n", n)
	})
	mux.HandleFunc("POST /acquire", func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Query().Get("tag")
		s.mu.Lock()
		var ids []int64
		for id, j := range s.available {
			if j.JobDisplayName == tag {
				ids = append(ids, id)
			}
		}
		s.mu.Unlock()
		got, err := s.sess.AcquireJobs(r.Context(), ids)
		s.log.Info("control: acquire", "tag", tag, "requested", ids, "acquired", got, "error", errStr(err))
		if err == nil {
			s.mu.Lock()
			for _, id := range got {
				delete(s.available, id)
			}
			s.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"requested": ids, "acquired": got, "error": errStr(err)})
	})
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		if err := s.start(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("POST /kill", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		s.mu.Lock()
		id := s.runners[name]
		s.mu.Unlock()
		err := s.dc.ContainerRemove(r.Context(), id, container.RemoveOptions{Force: true})
		s.log.Info("control: kill", "name", name, "error", errStr(err))
	})
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	go func() { _ = srv.Serve(ln) }()
	return nil
}
