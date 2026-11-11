package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const trendInterval = 10 * time.Second

// memSample is one reading of the machine's memory headroom: how much
// memory is available, and how much of the last ten seconds running tasks
// already spent stalled on memory (PSI "some"). A multi-GiB pull
// decompresses layers (observed peak ~4 GiB), so both numbers gate it.
type memSample struct {
	AvailBytes uint64
	PSISome10  float64
}

// reason explains why the sample is out of budget; "" when it is within.
func (s memSample) reason(minAvail uint64, maxPSI float64) string {
	switch {
	case s.AvailBytes < minAvail:
		return fmt.Sprintf("only %s memory available, need %s", humanBytes(s.AvailBytes), humanBytes(minAvail))
	case s.PSISome10 > maxPSI:
		return fmt.Sprintf("memory PSI some avg10 %.1f%% > %.1f%%", s.PSISome10, maxPSI)
	}
	return ""
}

// readMemSample reads /proc. PSI is optional: a kernel without pressure
// stall information reports 0, and MemAvailable alone carries the gate.
func readMemSample() (memSample, error) {
	var s memSample
	avail, err := memAvailableBytes("/proc/meminfo")
	if err != nil {
		return s, fmt.Errorf("reading MemAvailable: %w", err)
	}
	s.AvailBytes = avail
	if psi, err := psiSomeAvg10("/proc/pressure/memory"); err == nil {
		s.PSISome10 = psi
	}
	return s, nil
}

func memAvailableBytes(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing %q: %w", line, err)
			}
			return kb * 1024, nil
		}
	}
	return 0, errors.New("no MemAvailable line in " + path)
}

func psiSomeAvg10(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line)[1:] {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				p, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return 0, fmt.Errorf("parsing %q: %w", f, err)
				}
				return p, nil
			}
		}
	}
	return 0, errors.New("no some/avg10 in " + path)
}

// watchHeadroom samples the window dur at the given cadence and fails as
// soon as one sample leaves the budget: a machine busy at any moment of
// the window is not idle enough for a pull. dur==0 takes a single sample.
// The window is count-driven (dur/interval + 1 samples, sleeping interval
// between them), so tests can pass an instant sleep; it is also what makes
// the cadence exact instead of drift-prone wall-clock arithmetic.
func watchHeadroom(dur, interval time.Duration, minAvail uint64, maxPSI float64, sample func() (memSample, error), sleep func(time.Duration)) (bool, memSample, error) {
	steps := 0
	if dur > 0 && interval > 0 {
		steps = int(dur / interval)
	}
	var last memSample
	for i := 0; i <= steps; i++ {
		s, err := sample()
		if err != nil {
			return false, last, err
		}
		last = s
		if s.reason(minAvail, maxPSI) != "" {
			return false, s, nil
		}
		if i < steps {
			sleep(interval)
		}
	}
	return true, last, nil
}
