package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// runner is the seam between the update logic and podman. Tests fake it;
// production uses execRunner. Keeping it this small is deliberate: the
// updater has no interest in podman's API, only in a handful of commands.
type runner interface {
	output(argv ...string) ([]byte, error)
	run(argv ...string) error
}

type execRunner struct{}

func (execRunner) output(argv ...string) ([]byte, error) {
	cmd := exec.Command("podman", argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return out, fmt.Errorf("podman %s: %v: %s", strings.Join(argv, " "), err, msg)
		}
		return out, fmt.Errorf("podman %s: %v", strings.Join(argv, " "), err)
	}
	return out, nil
}

// run streams stdout and stderr through, so pull progress lands in the
// journal next to the updater's own log lines.
func (execRunner) run(argv ...string) error {
	cmd := exec.Command("podman", argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("podman %s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// localDigest returns the digest of the locally tagged image, "" when the
// image is absent (first run) or podman itself is unreachable (the pull
// that follows will fail and be recorded, so this needs no distinction).
func localDigest(r runner, ref string) string {
	out, err := r.output("image", "inspect", "-f", "{{.Digest}}", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func imageID(r runner, ref string) string {
	out, err := r.output("image", "inspect", "-f", "{{.Id}}", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runningNames(r runner) ([]string, error) {
	out, err := r.output("ps", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// imageLabel reads one label off an image (by reference or ID); "" when the
// image is absent, the label is missing, or the output is not JSON.
func imageLabel(r runner, refOrID string, key string) string {
	out, err := r.output("image", "inspect", "-f", "{{json .Labels}}", refOrID)
	if err != nil {
		return ""
	}
	var labels map[string]string
	if json.Unmarshal(out, &labels) != nil {
		return ""
	}
	return labels[key]
}

// danglingImages lists the IDs of untagged images. In-use ones may or may
// not appear depending on the podman version; the caller's rmi handles both.
func danglingImages(r runner) ([]string, error) {
	out, err := r.output("images", "--filter", "dangling=true", "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// freeBytes reports the space available to an unprivileged user on the
// filesystem holding path. Rootless podman's storage lives on the same
// volume as $HOME, which is why the default gate checks $HOME. An unreadable
// path returns MaxUint64: an unknown disk must not block an update.
func freeBytes(path string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return ^uint64(0)
	}
	return st.Bavail * uint64(st.Bsize)
}
