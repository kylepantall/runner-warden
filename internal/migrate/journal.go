package migrate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Journal statuses.
const (
	StepOK      = "ok"
	StepPending = "pending" // will be retried on the next run
	StepError   = "error"
)

// JournalEntry is one line of the append-only journal.
type JournalEntry struct {
	Time   time.Time `json:"time"`
	Runner string    `json:"runner"`
	Step   string    `json:"step"`
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
}

// Journal is an append-only JSON-lines log; completed steps are replayed on
// open so a re-run resumes instead of repeating.
type Journal struct {
	path string
	mu   sync.Mutex
	done map[string]bool
	now  func() time.Time
}

func stepKey(runner, step string) string { return runner + "\x00" + step }

// OpenJournal opens (creating if needed) the journal at path.
func OpenJournal(path string, now func() time.Time) (*Journal, error) {
	if now == nil {
		now = time.Now
	}
	j := &Journal{path: path, done: map[string]bool{}, now: now}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("journal dir: %w", err)
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e JournalEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // torn last line from a crash
		}
		if e.Status == StepOK {
			j.done[stepKey(e.Runner, e.Step)] = true
		}
	}
	return j, sc.Err()
}

// Done reports whether a step already completed.
func (j *Journal) Done(runner, step string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done[stepKey(runner, step)]
}

// Append writes an entry and syncs it.
func (j *Journal) Append(runner, step, status, detail string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, err := json.Marshal(JournalEntry{Time: j.now().UTC(), Runner: runner, Step: step, Status: status, Detail: detail})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("append journal: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append journal: %w", err)
	}
	if status == StepOK {
		j.done[stepKey(runner, step)] = true
	}
	return f.Sync()
}

// Entries reads every entry (for status output and tests).
func (j *Journal) Entries() ([]JournalEntry, error) {
	f, err := os.Open(j.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []JournalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e JournalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
