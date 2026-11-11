package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() (*logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return &logger{w: &buf}, &buf
}

func TestStateRoundTrip(t *testing.T) {
	lg, buf := testLogger()
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	in := State{}
	in.recordSuccess(time.Now(), "sha256:abc", true)
	in.recordFailure(time.Now(), errString("boom"))
	in.Version = 1 // saveState stamps this on the file, so the loaded state carries it

	saveState(path, in, lg)
	if buf.Len() > 0 {
		t.Fatalf("saveState logged unexpectedly: %s", buf)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	if raw["version"] != float64(1) {
		t.Errorf("version = %v, want 1", raw["version"])
	}

	lg, _ = testLogger()
	out := loadState(path, lg)
	if out != in {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestLoadStateMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()

	lg, buf := testLogger()
	if st := loadState(filepath.Join(dir, "absent.json"), lg); st != (State{Version: 1}) {
		t.Errorf("missing file: got %+v, want zero state", st)
	}
	if buf.Len() > 0 {
		t.Errorf("missing file should not warn, logged: %s", buf)
	}

	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lg, buf = testLogger()
	if st := loadState(badPath, lg); st != (State{Version: 1}) {
		t.Errorf("corrupt file: got %+v, want zero state", st)
	}
	if buf.Len() == 0 {
		t.Error("corrupt file should warn")
	}
}

func TestStatePerms(t *testing.T) {
	lg, _ := testLogger()
	path := filepath.Join(t.TempDir(), "state.json")
	saveState(path, State{}, lg)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestStaleReasons(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	day := func(n int) string { return now.AddDate(0, 0, -n).Format(time.RFC3339) }

	cases := []struct {
		name string
		st   State
		want int // number of reasons
	}{
		{"fresh state", State{}, 0},
		{"healthy success today", State{LastSuccess: day(0)}, 0},
		{"success inside the window", State{LastSuccess: day(6)}, 0},
		{"success outside the window", State{LastSuccess: day(8)}, 1},
		{"three failures", State{ConsecutiveFailures: 3, LastError: "net", LastSuccess: day(0)}, 1},
		{"failures and stale", State{ConsecutiveFailures: 4, LastError: "net", LastSuccess: day(9)}, 2},
		{"never succeeded, young", State{LastRun: day(2)}, 0},
		{"never succeeded, stale", State{LastRun: day(9)}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := staleReasons(c.st, now, 7)
			if len(got) != c.want {
				t.Errorf("staleReasons = %v, want %d reasons", got, c.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
