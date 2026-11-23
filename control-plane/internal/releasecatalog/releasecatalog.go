// Package releasecatalog loads the operator-provisioned trusted agent
// release manifest. It is the only source of package digests for single-node
// agent upgrades: callers never accept a digest, URL, or path from the
// network, and lookups are exact (version, architecture, native package format) tuples.
package releasecatalog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/GentleKingson/ocservia/control-plane/internal/semanticpayload"
)

// ErrInvalidManifest reports a manifest that failed fail-closed validation.
var ErrInvalidManifest = errors.New("agent release manifest is invalid")

// Release is one trusted (version, architecture, native package format) release identity.
type Release struct {
	Version       string `json:"version"`
	Architecture  string `json:"architecture"`
	PackageFormat string `json:"package_format"`
	PackageSHA256 string `json:"package_sha256"`
}

type manifestFile struct {
	Releases []Release `json:"releases"`
}

// Catalog is an immutable validated set of trusted release identities.
type Catalog struct {
	digests map[releaseKey][32]byte
}

type releaseKey struct {
	version      string
	architecture string
	format       string
}

// Load reads and validates the manifest at the fixed operator-provisioned
// path. A configured path that is missing, unreadable, malformed, or contains
// a duplicate (version, architecture, native package format) entry is a startup failure.
func Load(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrInvalidManifest, path, err)
	}
	var file manifestFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", ErrInvalidManifest, path, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: parse %s: trailing content", ErrInvalidManifest, path)
	}
	if len(file.Releases) == 0 || len(file.Releases) > 512 {
		return nil, fmt.Errorf("%w: %s must contain between 1 and 512 releases", ErrInvalidManifest, path)
	}
	catalog := &Catalog{digests: make(map[releaseKey][32]byte, len(file.Releases))}
	for _, release := range file.Releases {
		if release.PackageFormat != "deb" && release.PackageFormat != "rpm" {
			return nil, fmt.Errorf("%w: package_format must be deb or rpm", ErrInvalidManifest)
		}
		if !semanticpayload.ValidAgentUpgradeTargetVersion(release.Version) ||
			!semanticpayload.ValidAgentUpgradeArchitecture(release.Architecture) {
			return nil, fmt.Errorf("%w: release version or architecture is invalid", ErrInvalidManifest)
		}
		digest, decodeErr := decodeDigest(release.PackageSHA256)
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: release %s/%s digest is invalid", ErrInvalidManifest, release.Version, release.Architecture)
		}
		key := releaseKey{version: release.Version, architecture: release.Architecture, format: release.PackageFormat}
		if _, exists := catalog.digests[key]; exists {
			return nil, fmt.Errorf("%w: duplicate release %s/%s", ErrInvalidManifest, release.Version, release.Architecture)
		}
		catalog.digests[key] = digest
	}
	return catalog, nil
}

// Lookup resolves native packages using the authenticated node snapshot’s /etc/os-release ID.
func (c *Catalog) Lookup(version, architecture, osRelease string) ([32]byte, bool) {
	if c == nil {
		return [32]byte{}, false
	}
	var format string
	switch osRelease {
	case "debian", "ubuntu":
		format = "deb"
	case "rocky":
		format = "rpm"
	default:
		return [32]byte{}, false
	}
	digest, ok := c.digests[releaseKey{version: version, architecture: architecture, format: format}]
	return digest, ok
}

func decodeDigest(value string) ([32]byte, error) {
	var digest [32]byte
	if len(value) != 64 {
		return digest, errors.New("digest must be 64 hex characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return digest, errors.New("digest must be lowercase hex")
	}
	if value != hex.EncodeToString(decoded) {
		return [32]byte{}, errors.New("digest must be lowercase hex")
	}
	copy(digest[:], decoded)
	return digest, nil
}
