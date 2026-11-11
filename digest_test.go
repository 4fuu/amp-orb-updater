package main

import "testing"

// Real `podman manifest inspect --verbose` output for the amp-orb image
// (captured 2026-11-10): an OCI index with one linux/amd64 manifest and one
// attestation manifest (unknown/unknown) that must never be picked.
const ampOrbIndex = `{
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.index.v1+json",
    "manifests": [
        {
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "size": 6217,
            "digest": "sha256:96cfe95a102967092badc2b06e85680d68de35f4de51f7cc63b02b59a1205c9b",
            "platform": {
                "architecture": "amd64",
                "os": "linux"
            }
        },
        {
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "size": 837,
            "digest": "sha256:aa1138892dd387720827d3d422038d7cc5b4d52f44334ecbd58860fe7f3ec06f",
            "platform": {
                "architecture": "unknown",
                "os": "unknown"
            },
            "annotations": {
                "vnd.docker.reference.digest": "sha256:96cfe95a102967092badc2b06e85680d68de35f4de51f7cc63b02b59a1205c9b",
                "vnd.docker.reference.type": "attestation-manifest"
            }
        }
    ]
}`

func TestAmd64DigestPicksLinuxAmd64NotAttestation(t *testing.T) {
	want := "sha256:96cfe95a102967092badc2b06e85680d68de35f4de51f7cc63b02b59a1205c9b"
	if got := amd64Digest([]byte(ampOrbIndex)); got != want {
		t.Fatalf("amd64Digest = %q, want %q", got, want)
	}
}

func TestAmd64DigestUnknown(t *testing.T) {
	cases := []struct{ name, in string }{
		{"single manifest without an index", `{"schemaVersion":2,"config":{"digest":"sha256:cfg"},"layers":[]}`},
		{"index without linux/amd64", `{"manifests":[{"digest":"sha256:arm","platform":{"architecture":"arm64","os":"linux"}}]}`},
		{"amd64 entry without a sha256 digest", `{"manifests":[{"digest":"","platform":{"architecture":"amd64","os":"linux"}}]}`},
		{"podman error text", "Error: manifest unknown"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := amd64Digest([]byte(c.in)); got != "" {
				t.Errorf("amd64Digest = %q, want empty", got)
			}
		})
	}
}
