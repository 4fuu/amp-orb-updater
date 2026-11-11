package main

import (
	"fmt"
	"strings"
	"time"
)

type config struct {
	image        string
	statePath    string
	powerRoot    string // sysfs power_supply root for the AC gate
	homePath     string // filesystem for the free-space gate
	minFree      uint64
	prefix       string // running containers with this name prefix are logged as context
	maxStaleDays int
	dryRun       bool

	// Memory-headroom gate: a pull may only start when MemAvailable and
	// memory PSI have stayed within budget for the whole trend window.
	// sampleMem/sleepMem are seams for tests; nil means the real thing.
	minMemAvail uint64
	maxMemPSI   float64
	trend       time.Duration
	forceAfter  time.Duration // pending longer than this skips the memory gate
	sampleMem   func() (memSample, error)
	sleepMem    func(time.Duration)
}

type result struct {
	status  string // "unchanged", "updated", "pending", "checked", "failed"
	reasons []string
}

// runOnce performs one full update attempt. The order matters:
//
//  1. Best-effort registry precheck: when the remote linux/amd64 digest
//     already matches the local one, finish without touching any gate or
//     the network beyond one manifest fetch.
//  2. Gates. Battery and disk headroom are hard limits. Memory headroom is
//     measured as a trend — MemAvailable and memory PSI must stay within
//     budget for the whole window, because a pull decompresses ~4 GiB. A
//     pending update older than forceAfter pushes past the memory gate;
//     battery and disk always apply. When any gate blocks, the update stays
//     pending and the next timer tick retries.
//  3. Pull. Podman only moves the tag on success, so a failed pull leaves
//     the current image untouched.
//  4. Sweep superseded bases: dangling images carrying the same
//     org.opencontainers.image.source label as the current one are removed
//     without --force — if any container (running, paused, or a podman
//     commit snapshot) still references one, podman refuses and a later
//     run takes it once the container is gone.
func runOnce(cfg config, r runner, now time.Time, lg *logger) result {
	st := loadState(cfg.statePath, lg)

	local := localDigest(r, cfg.image)

	remote := ""
	if out, err := r.output("manifest", "inspect", "--verbose", cfg.image); err != nil {
		lg.warnf("precheck: podman manifest inspect failed: %v", err)
	} else if remote = amd64Digest(out); remote == "" {
		lg.warnf("precheck: no linux/amd64 entry in the index; a gated pull will decide")
	}

	if remote != "" && remote == local {
		lg.infof("image %s is up to date (digest %s)", cfg.image, short(local))
		st.recordSuccess(now, local, false)
		if !cfg.dryRun {
			saveState(cfg.statePath, st, lg)
			sweepSuperseded(r, cfg.image, lg)
		}
		reportStale(st, now, cfg.maxStaleDays, lg)
		return result{status: "unchanged"}
	}

	switch {
	case local == "":
		lg.infof("no local copy of %s; a pull is needed", cfg.image)
	case remote != "":
		lg.infof("update pending: %s -> %s", short(local), short(remote))
	}

	// Mark when this update was first seen; gates and the forced path
	// both read it, and a success clears it.
	if st.PendingSince == "" {
		st.PendingSince = now.Format(time.RFC3339)
	}

	if blocked := gatherGates(cfg, r, st, now, lg); len(blocked) > 0 {
		for _, b := range blocked {
			lg.infof("gate: %s", b)
		}
		st.LastRun = now.Format(time.RFC3339)
		if !cfg.dryRun {
			saveState(cfg.statePath, st, lg)
		}
		reportStale(st, now, cfg.maxStaleDays, lg)
		return result{status: "pending", reasons: blocked}
	}

	if cfg.dryRun {
		lg.infof("dry run: would pull %s now", cfg.image)
		return result{status: "checked"}
	}

	oldID := imageID(r, cfg.image)
	if err := r.run("pull", cfg.image); err != nil {
		st.recordFailure(now, err)
		saveState(cfg.statePath, st, lg)
		reportStale(st, now, cfg.maxStaleDays, lg)
		return result{status: "failed", reasons: []string{err.Error()}}
	}

	newID := imageID(r, cfg.image)
	changed := oldID != "" && oldID != newID
	if !changed {
		lg.infof("pull completed; image content unchanged (id %s)", short(newID))
	}

	st.recordSuccess(now, localDigest(r, cfg.image), changed)
	saveState(cfg.statePath, st, lg)
	sweepSuperseded(r, cfg.image, lg)
	reportStale(st, now, cfg.maxStaleDays, lg)
	status := "unchanged"
	if changed {
		status = "updated"
	}
	return result{status: status}
}

// sweepSuperseded removes dangling images that are earlier bases of the
// same image. A base superseded while an orb still referenced it could not
// be removed at update time; this sweep takes it on a later run once that
// container is gone. Candidates are identified by the
// org.opencontainers.image.source label of the current image, so dangling
// images belonging to other projects are never touched, and podman rmi
// still refuses anything a container uses.
func sweepSuperseded(r runner, ref string, lg *logger) {
	marker := imageLabel(r, ref, "org.opencontainers.image.source")
	if marker == "" {
		lg.infof("sweep: %s carries no org.opencontainers.image.source label; nothing to match", ref)
		return
	}
	ids, err := danglingImages(r)
	if err != nil {
		lg.warnf("sweep: listing dangling images: %v", err)
		return
	}
	for _, id := range ids {
		if imageLabel(r, id, "org.opencontainers.image.source") != marker {
			continue
		}
		if err := r.run("rmi", id); err != nil {
			lg.infof("sweep: %s still referenced; kept", short(id))
		} else {
			lg.infof("sweep: removed superseded image %s", short(id))
		}
	}
}

// gatherGates collects every reason the update must wait. Battery and disk
// are hard limits. The memory gate measures the machine instead of guessing
// from container counts: MemAvailable must cover the pull's decompression
// peak and memory PSI must stay calm — for the whole trend window, not just
// at this instant, because "idle now" says nothing about the minute before.
// A pending update older than forceAfter pushes past the memory gate only.
func gatherGates(cfg config, r runner, st State, now time.Time, lg *logger) []string {
	var reasons []string

	if ac, warn := acPower(cfg.powerRoot); !ac {
		reasons = append(reasons, "on battery power")
	} else if warn != "" {
		lg.warnf("gate: %s; assuming AC", warn)
	}

	if free := freeBytes(cfg.homePath); free < cfg.minFree {
		reasons = append(reasons, fmt.Sprintf("only %s free on %s, need %s", humanBytes(free), cfg.homePath, humanBytes(cfg.minFree)))
	}

	// Running orbs are context, not a gate: a pull never touches them, the
	// memory budget decides whether the machine has room for one.
	if names, err := runningNames(r); err == nil {
		n := 0
		for _, name := range names {
			if strings.HasPrefix(name, cfg.prefix) {
				n++
			}
		}
		if n > 0 {
			lg.infof("gate: %d orb(s) running; the memory-headroom gate decides, not the container count", n)
		}
	}

	if since, err := time.Parse(time.RFC3339, st.PendingSince); err == nil && now.Sub(since) > cfg.forceAfter {
		lg.infof("gate: pending since %s (> %s); forcing past the memory gate", st.PendingSince, cfg.forceAfter)
		return reasons
	}

	sample := cfg.sampleMem
	if sample == nil {
		sample = readMemSample
	}
	sleep := cfg.sleepMem
	if sleep == nil {
		sleep = time.Sleep
	}
	dur := cfg.trend
	if cfg.dryRun {
		lg.infof("gate: dry run samples once instead of watching for %s", dur)
		dur = 0
	} else if dur > 0 {
		lg.infof("gate: watching memory headroom for %s", dur)
	}
	ok, s, err := watchHeadroom(dur, trendInterval, cfg.minMemAvail, cfg.maxMemPSI, sample, sleep)
	switch {
	case err != nil:
		reasons = append(reasons, "cannot read memory headroom: "+err.Error())
	case !ok:
		reasons = append(reasons, s.reason(cfg.minMemAvail, cfg.maxMemPSI))
	default:
		lg.infof("gate: headroom held (avail %s, PSI some avg10 %.1f%%)", humanBytes(s.AvailBytes), s.PSISome10)
	}
	return reasons
}

func reportStale(st State, now time.Time, maxDays int, lg *logger) {
	for _, s := range staleReasons(st, now, maxDays) {
		lg.warnf("stale: %s", s)
	}
}

func short(s string) string {
	if len(s) > 19 {
		return s[:19]
	}
	return s
}

func humanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
