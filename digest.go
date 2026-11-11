package main

import (
	"encoding/json"
	"strings"
)

// imageIndex is the subset of an OCI index / Docker manifest list that
// `podman manifest inspect --verbose` prints. Anything else podman may
// print (a bare manifest for a single-arch tag, an error message) simply
// does not decode into .Manifests, and amd64Digest answers "unknown".
type imageIndex struct {
	Manifests []indexEntry `json:"manifests"`
}

type indexEntry struct {
	Digest   string        `json:"digest"`
	Platform indexPlatform `json:"platform"`
}

type indexPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// amd64Digest returns the digest of the linux/amd64 manifest inside the
// index. That is the manifest an amd64 host pulls, so it is also the digest
// podman reports for the locally tagged image — which is what makes the
// precheck comparison valid. Attestation entries (unknown/unknown platform)
// never match. "" means unknown; the caller then lets a gated pull decide.
func amd64Digest(out []byte) string {
	var idx imageIndex
	if err := json.Unmarshal(out, &idx); err != nil {
		return ""
	}
	for _, m := range idx.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" &&
			strings.HasPrefix(m.Digest, "sha256:") {
			return m.Digest
		}
	}
	return ""
}
