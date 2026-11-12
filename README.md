# amp-orb-updater

Keeps the base image of a self-hosted [Amp](https://ampcode.com/) orb
provider fresh:
`us-central1-docker.pkg.dev/amp-prod-0/amp-orb/amp-orb:latest`, run by
rootless podman through `~/.config/amp-orb-provider/dispatch.sh`.

The dispatch script deliberately uses `podman run --pull=missing` (local
image present means never re-pull), so nothing else would ever move the
tag. This program is that something else. It is a single static Go binary
with no dependencies outside the standard library, checked hourly around
the clock by a systemd user timer — the precheck is one cheap manifest
fetch, and the gates decide whether a pull may start.

## What one run does

1. **Precheck.** `podman manifest inspect --verbose` fetches the registry
   index; the `linux/amd64` entry's digest is compared with the local
   image's digest. Equal means up to date: done, one manifest fetch of
   traffic, gates never consulted. The comparison is valid because the
   amd64 manifest digest is exactly what podman stores as the local
   digest for a tag pulled on an amd64 host. A failed or unparseable
   precheck is not fatal; the pull decides instead.
2. **Gates.** Battery power (sysfs `power_supply`; a machine with no
   readable supplies counts as on AC) and disk headroom (≥12 GiB free on
   the filesystem holding `$HOME`) are hard limits. Pulling never touches
   running orbs — they keep their image by ID — so the remaining question
   is whether the machine has room for a ~4 GiB decompression, not who is
   using it. That is measured, not guessed from container counts:
   `MemAvailable` must be ≥ `--min-mem-avail-bytes` (default 8 GiB) and
   memory PSI (`some avg10`) ≤ `--max-mem-psi` (default 5%), and both must
   hold for the whole `--trend` window (default 2 minutes, sampled every
   10 s; one busy sample defers the update to the next tick). An update
   pending longer than `--force-after` (default 72 h) pushes past the
   memory gate; battery and disk always apply.
3. **Pull.** `podman pull` only moves the tag on success, so a failed
   pull leaves the running setup untouched.
4. **Sweep.** Dangling images carrying the same
   `org.opencontainers.image.source` label as the current image are
   removed **without** `--force`. That label is how an amp-orb base is
   recognized, so dangling images belonging to other projects are never
   touched — and if any container (running, paused, or a `podman commit`
   project snapshot) still references one, podman refuses and a later run
   takes it once the container is gone. A base superseded while an orb was
   paused therefore does not leak; it is collected later. The sweep runs on
   every successful run, including up-to-date ones. `podman image prune`
   is never used; it is too blunt for a store that also holds unrelated
   containers and snapshots.

State lives in `~/.local/state/amp-orb-updater/state.json` (atomic write,
0600) and feeds two warnings: 3+ consecutive failures, or no successful
run within `--max-stale-days` (default 7). A week of blocked gates is the
mechanism working, but it should be visible in the log, not silent.

## What it never does

- It never touches running orbs: they reference the old image by ID and
  keep running on it. New spawns pick up the new image. Orbs woken from
  a snapshot keep using the snapshot — that is Amp's resume mechanism,
  and a newer base image does not invalidate it.
- It never pulls inside the dispatch path. Dispatch answers Amp's
  callbacks under a 5-minute contract; this program answers a timer.
- No daemon. A timer with `Persistent=true` already handles sleep, boot,
  and missed windows; a resident process would only add reconnect logic.

## Install

```
make enable          # build, install to ~/.local/bin, install units, enable timer
```

Manual runs and inspection:

```
amp-orb-updater --check            # dry run: report, pull nothing, write no state
amp-orb-updater                    # real run now
systemctl --user list-timers amp-orb-updater.timer
journalctl --user -u amp-orb-updater.service
cat ~/.local/state/amp-orb-updater/state.json
```

After editing the code, `make install` rebuilds and refreshes the units;
the timer keeps running and picks up the new binary on the next tick.

## Tests

```
go test ./...
```

All tests run against a fake podman runner and fake sysfs trees; nothing
needs podman, the network, or the real image. The fixtures include the
actual `podman manifest inspect --verbose` output of the amp-orb image
(OCI index with an amd64 manifest and an attestation manifest that must
never be picked).

## Layout

| File | |
| --- | --- |
| `main.go` | flags, defaults, exit code |
| `updater.go` | one run: precheck, gates, pull, cleanup |
| `podman.go` | the `runner` seam and the podman commands |
| `digest.go` | amd64 manifest digest from a registry index |
| `power.go` | AC detection from sysfs |
| `resources.go` | memory headroom sampling and the trend watch |
| `state.go` | state file, staleness reasons, logger |
| `deploy/` | systemd user service + timer |

## License

MIT — see [LICENSE](LICENSE).
