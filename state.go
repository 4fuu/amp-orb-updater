package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// State is the small JSON record the updater keeps between runs. It exists
// so the staleness warnings can say "nothing succeeded for N days" instead
// of the timer silently no-op'ing forever while gates or errors pile up.
type State struct {
	Version             int    `json:"version"`
	LastRun             string `json:"last_run"`              // RFC3339 of the last attempt of any kind
	LastSuccess         string `json:"last_success"`          // RFC3339 of the last success, "" if never
	LastChange          string `json:"last_change,omitempty"` // RFC3339 of the last pull that moved the image
	LastDigest          string `json:"last_digest,omitempty"`
	PendingSince        string `json:"pending_since,omitempty"` // first observation of the current pending update
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
}

func (st *State) recordSuccess(now time.Time, digest string, changed bool) {
	st.LastRun = now.Format(time.RFC3339)
	st.LastSuccess = st.LastRun
	st.LastDigest = digest
	if changed {
		st.LastChange = st.LastRun
	}
	st.PendingSince = ""
	st.ConsecutiveFailures = 0
	st.LastError = ""
}

func (st *State) recordFailure(now time.Time, err error) {
	st.LastRun = now.Format(time.RFC3339)
	st.ConsecutiveFailures++
	st.LastError = err.Error()
}

func loadState(path string, lg *logger) State {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{Version: 1}
	}
	if err != nil {
		lg.warnf("state: reading %s: %v; starting fresh", path, err)
		return State{Version: 1}
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		lg.warnf("state: parsing %s: %v; starting fresh", path, err)
		return State{Version: 1}
	}
	return st
}

// saveState writes the state atomically (tmp file + rename) with 0600, so a
// crash mid-write never leaves a truncated record behind.
func saveState(path string, st State, lg *logger) {
	st.Version = 1
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		lg.warnf("state: creating %s: %v", filepath.Dir(path), err)
		return
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		lg.warnf("state: encoding: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		lg.warnf("state: writing %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		lg.warnf("state: renaming to %s: %v", path, err)
	}
}

// staleReasons lists why the updater considers itself unhealthy. Warnings,
// not failures: gates blocking for a week is the mechanism working as
// designed, but it should be visible.
func staleReasons(st State, now time.Time, maxDays int) []string {
	var out []string
	if st.ConsecutiveFailures >= 3 {
		out = append(out, fmt.Sprintf("%d consecutive update failures (last: %s)", st.ConsecutiveFailures, st.LastError))
	}
	limit := time.Duration(maxDays) * 24 * time.Hour
	if t, err := time.Parse(time.RFC3339, st.LastSuccess); err == nil {
		if d := now.Sub(t); d > limit {
			out = append(out, fmt.Sprintf("no successful run for %.0f days", d.Hours()/24))
		}
		return out
	}
	if t, err := time.Parse(time.RFC3339, st.LastRun); err == nil && now.Sub(t) > limit {
		out = append(out, fmt.Sprintf("running for %.0f days without a single success", now.Sub(t).Hours()/24))
	}
	return out
}

// logger keeps every line one grep-able "timestamp LEVEL message" record;
// under the timer these land in the journal, in the foreground they land on
// the terminal.
type logger struct{ w io.Writer }

func (l *logger) printf(level, format string, a ...any) {
	fmt.Fprintf(l.w, "%s %s %s\n", time.Now().UTC().Format(time.RFC3339), level, fmt.Sprintf(format, a...))
}

func (l *logger) infof(format string, a ...any)  { l.printf("INFO", format, a...) }
func (l *logger) warnf(format string, a ...any)  { l.printf("WARN", format, a...) }
func (l *logger) errorf(format string, a ...any) { l.printf("ERROR", format, a...) }
