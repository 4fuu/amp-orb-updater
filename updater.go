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
	prefix       string // running containers with this name prefix block the pull
	maxStaleDays int
	dryRun       bool
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
//  2. Gates (battery, running orbs, disk headroom). When any blocks, the
//     update stays pending and the next timer tick retries.
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

	if blocked := gatherGates(cfg, r, lg); len(blocked) > 0 {
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

// gatherGates collects every reason the update must wait. Cheap local checks
// first; podman ps last because it takes the store lock.
func gatherGates(cfg config, r runner, lg *logger) []string {
	var reasons []string

	if ac, warn := acPower(cfg.powerRoot); !ac {
		reasons = append(reasons, "on battery power")
	} else if warn != "" {
		lg.warnf("gate: %s; assuming AC", warn)
	}

	if names, err := runningNames(r); err != nil {
		reasons = append(reasons, "cannot list running containers: "+err.Error())
	} else {
		for _, n := range names {
			if strings.HasPrefix(n, cfg.prefix) {
				reasons = append(reasons, fmt.Sprintf("orb %s is running", n))
				break
			}
		}
	}

	if free := freeBytes(cfg.homePath); free < cfg.minFree {
		reasons = append(reasons, fmt.Sprintf("only %s free on %s, need %s", humanBytes(free), cfg.homePath, humanBytes(cfg.minFree)))
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
