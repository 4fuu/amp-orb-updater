package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner scripts podman by exact argv: each key maps to a FIFO of
// responses, so "before pull / after pull" outputs are just two entries.
type fakeRunner struct {
	stdouts map[string][]string
	errs    map[string][]error
	calls   []string
}

func (f *fakeRunner) output(argv ...string) ([]byte, error) {
	key := strings.Join(argv, " ")
	f.calls = append(f.calls, "output "+key)
	out, err := f.next(key)
	return []byte(out), err
}

func (f *fakeRunner) run(argv ...string) error {
	key := strings.Join(argv, " ")
	f.calls = append(f.calls, "run "+key)
	_, err := f.next(key)
	return err
}

func (f *fakeRunner) next(key string) (string, error) {
	var out string
	if q := f.stdouts[key]; len(q) > 0 {
		out, f.stdouts[key] = q[0], q[1:]
	}
	var err error
	if q := f.errs[key]; len(q) > 0 {
		err, f.errs[key] = q[0], q[1:]
	}
	return out, err
}

func (f *fakeRunner) did(call string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, call) {
			return true
		}
	}
	return false
}

const (
	testImage = "reg.example/amp-orb:latest"
	digestOld = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestNew = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var (
	idOld   = "sha256:" + strings.Repeat("c", 64)
	idNew   = "sha256:" + strings.Repeat("d", 64)
	idOther = "sha256:" + strings.Repeat("e", 64) // dangling image from an unrelated project
)

const ampSourceLabel = `{"org.opencontainers.image.source":"https://github.com/ampcode/amp"}`

// plug wires the standard scenario: local image at digestOld, registry at
// digestNew (or equal when up-to-date is wanted). inspect queues hold both
// the pre-pull and post-pull values. The dangling sweep finds idOld (same
// source label) and idOther (different label, must be left alone).
func plug(f *fakeRunner, remoteDigest string) {
	f.stdouts = map[string][]string{
		"manifest inspect --verbose " + testImage:        {remoteManifest(remoteDigest)},
		"image inspect -f {{.Digest}} " + testImage:      {digestOld, digestNew},
		"image inspect -f {{.Id}} " + testImage:          {idOld, idNew},
		"ps --format {{.Names}}":                         {""},
		"image inspect -f {{json .Labels}} " + testImage: {ampSourceLabel},
		"images --filter dangling=true --format {{.ID}}": {idOld + "\n" + idOther},
		"image inspect -f {{json .Labels}} " + idOld:     {ampSourceLabel},
		"image inspect -f {{json .Labels}} " + idOther:   {`{"org.opencontainers.image.source":"https://github.com/other/project"}`},
	}
	f.errs = map[string][]error{}
}

func remoteManifest(digest string) string {
	if digest == "" {
		return "not a manifest list"
	}
	return `{"manifests":[{"digest":"` + digest + `","platform":{"architecture":"amd64","os":"linux"}}]}`
}

func testConfig(t *testing.T) config {
	return config{
		image:        testImage,
		statePath:    filepath.Join(t.TempDir(), "state.json"),
		powerRoot:    acRoot(t, "1"),
		homePath:     t.TempDir(),
		minFree:      1, // any real temp filesystem passes
		prefix:       "amp-",
		maxStaleDays: 7,
		minMemAvail:  1 << 30,
		maxMemPSI:    5.0,
		trend:        0, // tests sample once unless a case sets a window
		forceAfter:   72 * time.Hour,
		sampleMem: func() (memSample, error) {
			return memSample{AvailBytes: 16 << 30, PSISome10: 0}, nil
		},
		sleepMem: func(time.Duration) {},
	}
}

func acRoot(t *testing.T, online string) string {
	t.Helper()
	root := t.TempDir()
	writeSupply(t, root, "Mains", map[string]string{"type": "Mains", "online": online})
	return root
}

func run(t *testing.T, cfg config, f *fakeRunner) (result, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	res := runOnce(cfg, f, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), &logger{w: &buf})
	return res, &buf
}

func TestUpToDateShortCircuitsWithoutPull(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestOld) // remote digest == local digest
	cfg := testConfig(t)

	res, _ := run(t, cfg, f)
	if res.status != "unchanged" {
		t.Fatalf("status = %s, want unchanged", res.status)
	}
	if f.did("pull") {
		t.Error("pulled even though the digests matched")
	}
	// The unchanged path still sweeps: an old base kept behind by a paused
	// orb on a previous update gets removed here once nothing uses it.
	if !f.did("rmi " + idOld) {
		t.Error("expected the dangling superseded base to be swept")
	}
	st := loadState(cfg.statePath, &logger{w: &bytes.Buffer{}})
	if st.LastSuccess == "" || st.LastDigest != digestOld {
		t.Errorf("success not recorded: %+v", st)
	}
}

func TestPendingUpdateGatedByBattery(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	root := t.TempDir()
	writeSupply(t, root, "Mains", map[string]string{"type": "Mains", "online": "0"})
	writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Discharging"})
	cfg.powerRoot = root

	res, log := run(t, cfg, f)
	if res.status != "pending" {
		t.Fatalf("status = %s, want pending", res.status)
	}
	if f.did("pull") {
		t.Error("pulled while on battery")
	}
	if !strings.Contains(log.String(), "battery") {
		t.Errorf("log does not mention the battery gate:\n%s", log)
	}
	st := loadState(cfg.statePath, &logger{w: &bytes.Buffer{}})
	if st.LastSuccess != "" {
		t.Errorf("a gated run must not record success: %+v", st)
	}
	if st.LastRun == "" {
		t.Error("a gated run should still record last_run")
	}
}

func TestRunningOrbsDoNotBlockWhenHeadroomHolds(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	f.stdouts["ps --format {{.Names}}"] = []string{"amp-T-01a1c625-0000\namp-T-01a1a12a-0000\n"}
	cfg := testConfig(t)

	res, log := run(t, cfg, f)
	if res.status != "updated" {
		t.Fatalf("status = %s, want updated (orb count is context, not a gate)", res.status)
	}
	if !strings.Contains(log.String(), "the memory-headroom gate decides") {
		t.Errorf("log should name the deciding gate:\n%s", log)
	}
}

func TestUnrelatedContainersDoNotBlock(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	f.stdouts["ps --format {{.Names}}"] = []string{"personal-bot-bot\nqq-gui\n"}
	cfg := testConfig(t)

	res, _ := run(t, cfg, f)
	if res.status != "updated" {
		t.Fatalf("status = %s, want updated", res.status)
	}
}

func TestLowMemoryBlocks(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	cfg.sampleMem = func() (memSample, error) {
		return memSample{AvailBytes: 1 << 20, PSISome10: 0}, nil // 1 MiB
	}

	res, log := run(t, cfg, f)
	if res.status != "pending" {
		t.Fatalf("status = %s, want pending", res.status)
	}
	if f.did("pull") {
		t.Error("pulled without memory headroom")
	}
	if !strings.Contains(log.String(), "memory available") {
		t.Errorf("log should give the memory reason:\n%s", log)
	}
}

func TestPressureTrendBlocksOnAnyBusySample(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	cfg.trend = 30 * time.Second
	samples := []memSample{
		{AvailBytes: 16 << 30, PSISome10: 0},
		{AvailBytes: 16 << 30, PSISome10: 1},
		{AvailBytes: 16 << 30, PSISome10: 80}, // busy moment inside the window
		{AvailBytes: 16 << 30, PSISome10: 0},
	}
	i := 0
	cfg.sampleMem = func() (memSample, error) {
		if i >= len(samples) {
			t.Fatal("watch kept sampling past the failing sample")
		}
		s := samples[i]
		i++
		return s, nil
	}

	res, log := run(t, cfg, f)
	if res.status != "pending" {
		t.Fatalf("status = %s, want pending", res.status)
	}
	if f.did("pull") {
		t.Error("pulled although the trend window caught pressure")
	}
	if !strings.Contains(log.String(), "PSI some avg10 80.0%") {
		t.Errorf("log should name the failing sample:\n%s", log)
	}
}

func TestForceAfterPushesPastTheMemoryGate(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	cfg.sampleMem = func() (memSample, error) {
		return memSample{AvailBytes: 1 << 20, PSISome10: 99}, nil
	}
	stale := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).Add(-96 * time.Hour)
	saveState(cfg.statePath, State{Version: 1, PendingSince: stale.Format(time.RFC3339)}, &logger{w: &bytes.Buffer{}})

	res, log := run(t, cfg, f)
	if res.status != "updated" {
		t.Fatalf("status = %s, want updated (pending > forceAfter must push through)", res.status)
	}
	if !strings.Contains(log.String(), "forcing past the memory gate") {
		t.Errorf("log should announce the forced path:\n%s", log)
	}
}

func TestBatteryStillBlocksWhenForced(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	root := t.TempDir()
	writeSupply(t, root, "Mains", map[string]string{"type": "Mains", "online": "0"})
	writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Discharging"})
	cfg.powerRoot = root
	stale := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).Add(-96 * time.Hour)
	saveState(cfg.statePath, State{Version: 1, PendingSince: stale.Format(time.RFC3339)}, &logger{w: &bytes.Buffer{}})

	res, _ := run(t, cfg, f)
	if res.status != "pending" {
		t.Fatalf("status = %s, want pending (battery is a hard gate even when forced)", res.status)
	}
	if f.did("pull") {
		t.Error("pulled on battery despite the force rule")
	}
}

func TestPendingSinceMarkedAndCleared(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	cfg.sampleMem = func() (memSample, error) {
		return memSample{AvailBytes: 1 << 20, PSISome10: 0}, nil // blocks
	}

	run(t, cfg, f)
	st := loadState(cfg.statePath, &logger{w: &bytes.Buffer{}})
	if st.PendingSince == "" {
		t.Fatal("a blocked update must record pending_since")
	}

	// Next run with headroom: the pull succeeds and clears the marker.
	cfg.sampleMem = func() (memSample, error) {
		return memSample{AvailBytes: 16 << 30, PSISome10: 0}, nil
	}
	f2 := &fakeRunner{}
	plug(f2, digestNew)
	if res, _ := run(t, cfg, f2); res.status != "updated" {
		t.Fatalf("status = %s, want updated", res.status)
	}
	st = loadState(cfg.statePath, &logger{w: &bytes.Buffer{}})
	if st.PendingSince != "" {
		t.Errorf("pending_since must be cleared by success: %+v", st)
	}
}

func TestLowDiskBlocks(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	cfg.minFree = ^uint64(0) // no filesystem has this much free

	res, _ := run(t, cfg, f)
	if res.status != "pending" {
		t.Fatalf("status = %s, want pending", res.status)
	}
	if f.did("pull") {
		t.Error("pulled with no disk headroom")
	}
}

func TestPullUpdatesStateAndSweepsOldBasesOnly(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)

	res, log := run(t, cfg, f)
	if res.status != "updated" {
		t.Fatalf("status = %s, want updated", res.status)
	}
	if !f.did("pull " + testImage) {
		t.Error("expected a pull")
	}
	if !f.did("rmi " + idOld) {
		t.Error("expected the superseded amp-orb base to be removed")
	}
	if f.did("rmi " + idOther) {
		t.Error("a dangling image from another project must never be removed")
	}
	if !strings.Contains(log.String(), "update pending") {
		t.Errorf("log lacks the pending line:\n%s", log)
	}
	st := loadState(cfg.statePath, &logger{w: &bytes.Buffer{}})
	if st.LastDigest != digestNew || st.LastChange == "" || st.LastSuccess == "" {
		t.Errorf("state after update = %+v", st)
	}
}

func TestOldImageKeptWhenStillReferenced(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	f.errs["rmi "+idOld] = []error{errors.New("image is in use by container amp-old")}
	cfg := testConfig(t)

	res, log := run(t, cfg, f)
	if res.status != "updated" {
		t.Fatalf("a refused rmi must not fail the run: %s", res.status)
	}
	if !strings.Contains(log.String(), "still referenced") {
		t.Errorf("log should say the old image was kept:\n%s", log)
	}
}

func TestSweepSkippedWithoutSourceLabel(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestOld) // up to date, so the run succeeds and reaches the sweep
	f.stdouts["image inspect -f {{json .Labels}} "+testImage] = []string{`{"some":"other label"}`}
	cfg := testConfig(t)

	res, log := run(t, cfg, f)
	if res.status != "unchanged" {
		t.Fatalf("status = %s, want unchanged", res.status)
	}
	if f.did("rmi ") {
		t.Error("without the source label marker nothing may be removed")
	}
	if !strings.Contains(log.String(), "no org.opencontainers.image.source label") {
		t.Errorf("log should explain the skipped sweep:\n%s", log)
	}
}

func TestPullFailureRecorded(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	pullErr := errors.New("network is down")
	f.errs["pull "+testImage] = []error{pullErr}
	cfg := testConfig(t)

	res, _ := run(t, cfg, f)
	if res.status != "failed" {
		t.Fatalf("status = %s, want failed", res.status)
	}
	st := loadState(cfg.statePath, &logger{w: &bytes.Buffer{}})
	if st.ConsecutiveFailures != 1 || st.LastError != pullErr.Error() || st.LastSuccess != "" {
		t.Errorf("failure not recorded correctly: %+v", st)
	}
}

func TestUnknownRemoteDigestStillPullsWhenIdle(t *testing.T) {
	f := &fakeRunner{}
	plug(f, "") // manifest inspect returns something unparseable
	cfg := testConfig(t)

	res, log := run(t, cfg, f)
	if res.status != "updated" {
		t.Fatalf("status = %s, want updated (pull is the check and content moved)", res.status)
	}
	if !f.did("pull " + testImage) {
		t.Error("an unknown remote digest must fall through to a gated pull")
	}
	if !strings.Contains(log.String(), "gated pull will decide") {
		t.Errorf("log should explain the fallback:\n%s", log)
	}
}

func TestPrecheckErrorFallsThroughToPull(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	f.errs["manifest inspect --verbose "+testImage] = []error{errors.New("dial tcp: timeout")}
	cfg := testConfig(t)

	res, _ := run(t, cfg, f)
	if res.status != "updated" {
		t.Fatalf("status = %s, want updated (precheck failure must not block)", res.status)
	}
}

func TestDryRunPullsAndWritesNothing(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	cfg.dryRun = true

	res, _ := run(t, cfg, f)
	if res.status != "checked" {
		t.Fatalf("status = %s, want checked", res.status)
	}
	if f.did("pull") || f.did("rmi") {
		t.Error("dry run must not pull or remove anything")
	}
	if _, err := os.Stat(cfg.statePath); !os.IsNotExist(err) {
		t.Error("dry run must not write state")
	}
}

func TestFirstRunWithoutLocalImagePulls(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	f.errs["image inspect -f {{.Digest}} "+testImage] = []error{errors.New("not found")}
	f.errs["image inspect -f {{.Id}} "+testImage] = []error{errors.New("not found")}
	// After the pull the image exists: the second queue entries serve the
	// post-pull inspect calls, so no errors remain for them.
	cfg := testConfig(t)

	res, _ := run(t, cfg, f)
	if res.status != "unchanged" {
		t.Fatalf("status = %s, want unchanged (nothing was superseded)", res.status)
	}
	if !f.did("pull " + testImage) {
		t.Error("a missing local image must be pulled")
	}
	st := loadState(cfg.statePath, &logger{w: &bytes.Buffer{}})
	if st.LastDigest != digestNew {
		t.Errorf("state after first pull = %+v", st)
	}
}

func TestStaleWarningAfterBlockedWeek(t *testing.T) {
	f := &fakeRunner{}
	plug(f, digestNew)
	cfg := testConfig(t)
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Discharging"})
	cfg.powerRoot = root

	old := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	saveState(cfg.statePath, State{
		Version:     1,
		LastRun:     old.Format(time.RFC3339),
		LastSuccess: old.Format(time.RFC3339),
	}, &logger{w: &bytes.Buffer{}})

	res, log := run(t, cfg, f)
	if res.status != "pending" {
		t.Fatalf("status = %s, want pending", res.status)
	}
	if !strings.Contains(log.String(), "stale") {
		t.Errorf("a week of blocked runs should raise the stale warning:\n%s", log)
	}
}
