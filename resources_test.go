package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const meminfoFixture = `MemTotal:       31351520 kB
MemFree:        12345678 kB
MemAvailable:   23456789 kB
Buffers:          123456 kB
`

const psiFixture = `some avg10=0.00 avg60=0.05 avg300=0.02 total=62796396
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
`

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMemAvailableBytes(t *testing.T) {
	got, err := memAvailableBytes(writeFixture(t, "meminfo", meminfoFixture))
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(23456789) * 1024; got != want {
		t.Errorf("memAvailableBytes = %d, want %d", got, want)
	}
}

func TestMemAvailableErrors(t *testing.T) {
	if _, err := memAvailableBytes(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("missing file should error")
	}
	if _, err := memAvailableBytes(writeFixture(t, "meminfo", "MemFree: 100 kB\n")); err == nil {
		t.Error("no MemAvailable line should error")
	}
}

func TestPsiSomeAvg10(t *testing.T) {
	got, err := psiSomeAvg10(writeFixture(t, "memory", psiFixture))
	if err != nil {
		t.Fatal(err)
	}
	if got != 0.0 {
		t.Errorf("psiSomeAvg10 = %v, want 0", got)
	}

	got, err = psiSomeAvg10(writeFixture(t, "memory", "some avg10=7.25 avg60=9 total=1\nfull avg10=0.00 avg60=0 total=0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 7.25 {
		t.Errorf("psiSomeAvg10 = %v, want 7.25", got)
	}
}

func TestPsiSomeAvg10Errors(t *testing.T) {
	if _, err := psiSomeAvg10(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("missing file should error")
	}
	if _, err := psiSomeAvg10(writeFixture(t, "memory", "full avg10=1.00 avg60=1 total=0\n")); err == nil {
		t.Error("missing some line should error")
	}
}

func TestSampleReasonBoundaries(t *testing.T) {
	cases := []struct {
		name                   string
		s                      memSample
		minAvail               uint64
		maxPSI                 float64
		wantBlock              bool
		wantMentionsAvailFirst bool
	}{
		{"exactly at both budgets", memSample{AvailBytes: 8 << 30, PSISome10: 5.0}, 8 << 30, 5.0, false, false},
		{"below availability", memSample{AvailBytes: 7 << 30, PSISome10: 90}, 8 << 30, 5.0, true, true},
		{"above PSI only", memSample{AvailBytes: 16 << 30, PSISome10: 5.1}, 8 << 30, 5.0, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.s.reason(c.minAvail, c.maxPSI)
			if (r != "") != c.wantBlock {
				t.Errorf("reason = %q, blocked = %v, want %v", r, r != "", c.wantBlock)
			}
		})
	}
}

func TestWatchHeadroomCountsWindow(t *testing.T) {
	samples := 0
	sample := func() (memSample, error) {
		samples++
		return memSample{AvailBytes: 16 << 30, PSISome10: 0}, nil
	}
	sleeps := 0
	sleep := func(time.Duration) { sleeps++ }

	// A 2-minute window at 10 s cadence samples at t=0..120: 13 samples,
	// 12 sleeps.
	ok, s, err := watchHeadroom(2*time.Minute, trendInterval, 8<<30, 5.0, sample, sleep)
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v, want clean pass", ok, err)
	}
	if s.AvailBytes != 16<<30 {
		t.Errorf("returned the wrong sample: %+v", s)
	}
	if samples != 13 || sleeps != 12 {
		t.Errorf("samples=%d sleeps=%d, want 13/12", samples, sleeps)
	}
}

func TestWatchHeadroomSingleSampleWithoutWindow(t *testing.T) {
	samples, sleeps := 0, 0
	ok, _, err := watchHeadroom(0, trendInterval, 8<<30, 5.0,
		func() (memSample, error) { samples++; return memSample{AvailBytes: 16 << 30}, nil },
		func(time.Duration) { sleeps++ })
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if samples != 1 || sleeps != 0 {
		t.Errorf("samples=%d sleeps=%d, want 1/0", samples, sleeps)
	}
}

func TestWatchHeadroomFailsFast(t *testing.T) {
	n := 0
	ok, s, err := watchHeadroom(time.Minute, trendInterval, 8<<30, 5.0,
		func() (memSample, error) {
			n++
			if n == 3 {
				return memSample{AvailBytes: 1 << 30, PSISome10: 0}, nil // busy moment
			}
			return memSample{AvailBytes: 16 << 30, PSISome10: 0}, nil
		},
		func(time.Duration) {})
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v, want fast fail", ok, err)
	}
	if n != 3 {
		t.Errorf("took %d samples, want 3 (fail fast)", n)
	}
	if s.AvailBytes != 1<<30 {
		t.Errorf("returned the offending sample: %+v", s)
	}
}

func TestWatchHeadroomSamplerError(t *testing.T) {
	boom := errors.New("no /proc")
	ok, _, err := watchHeadroom(time.Minute, trendInterval, 8<<30, 5.0,
		func() (memSample, error) { return memSample{}, boom },
		func(time.Duration) {})
	if ok || !errors.Is(err, boom) {
		t.Fatalf("ok=%v err=%v, want the sampler error", ok, err)
	}
}

func TestReadMemSampleOnThisMachine(t *testing.T) {
	// The host always has /proc/meminfo; this doubles as a canary for the
	// parser against the real kernel format.
	s, err := readMemSample()
	if err != nil {
		t.Skipf("host has no readable /proc: %v", err)
	}
	if s.AvailBytes == 0 {
		t.Errorf("MemAvailable parsed as 0: %+v", s)
	}
}
