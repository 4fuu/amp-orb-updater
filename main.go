// amp-orb-updater keeps the base image of the self-hosted amp orb provider
// fresh. It is run by a systemd user timer (see deploy/): it never touches
// running orbs, pulls a newer image only when the machine is idle enough,
// and leaves the superseded image in place for as long as any container
// still references it.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"
)

const defaultImage = "us-central1-docker.pkg.dev/amp-prod-0/amp-orb/amp-orb:latest"

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	cfg := config{
		image:        defaultImage,
		statePath:    filepath.Join(home, ".local/state/amp-orb-updater/state.json"),
		powerRoot:    "/sys/class/power_supply",
		homePath:     home,
		minFree:      12 << 30,
		prefix:       "amp-",
		maxStaleDays: 7,
	}
	flag.StringVar(&cfg.image, "image", cfg.image, "image reference to keep fresh")
	flag.StringVar(&cfg.statePath, "state", cfg.statePath, "state file path")
	flag.StringVar(&cfg.powerRoot, "power-root", cfg.powerRoot, "sysfs power_supply root used for AC detection")
	flag.StringVar(&cfg.homePath, "disk-path", cfg.homePath, "filesystem whose free space gates the pull")
	flag.Uint64Var(&cfg.minFree, "min-free-bytes", cfg.minFree, "minimum free bytes required to pull")
	flag.StringVar(&cfg.prefix, "prefix", cfg.prefix, "running containers whose name has this prefix count as busy")
	flag.IntVar(&cfg.maxStaleDays, "max-stale-days", cfg.maxStaleDays, "warn when no successful run happened within this many days")
	flag.BoolVar(&cfg.dryRun, "check", false, "dry run: report what would happen, pull nothing, write no state")
	flag.Parse()

	lg := &logger{w: os.Stderr}
	res := runOnce(cfg, execRunner{}, time.Now(), lg)
	lg.infof("result: %s", res.status)
	for _, reason := range res.reasons {
		lg.infof("  %s", reason)
	}
	if res.status == "failed" {
		os.Exit(1)
	}
}
