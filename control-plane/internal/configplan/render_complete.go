package configplan

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/google/uuid"
)

const MaxCandidateBytes = 256 * 1024

var (
	ErrInvalid   = errors.New("configuration plan is invalid")
	labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Directive is one typed Ocserv directive. Exactly one of Value and SecretRef is set.
type Directive struct {
	Name      string     `json:"name"`
	Value     string     `json:"value,omitempty"`
	SecretRef *SecretRef `json:"secret_ref,omitempty"`
}

// SecretRef names secret material without resolving or storing it.
type SecretRef struct {
	ID       uuid.UUID `json:"secret_ref_id"`
	Provider string    `json:"-"`
	Key      string    `json:"-"`
	Version  string    `json:"-"`
}

// Template is a bounded, non-executable configuration document.
type Template struct {
	Name       string      `json:"name"`
	Directives []Directive `json:"directives"`
}

type RenderInput struct {
	Template      Template `json:"template"`
	OcservVersion string   `json:"ocserv_version"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

type Rendered struct {
	CompleteCandidate    *agentv1.CompleteConfigCandidate
	Candidate            []byte
	Redacted             string
	Hash                 [32]byte
	Warnings             []string
	RequiredCapabilities []string
}

func renderComplete(input RenderInput, nodeID uuid.UUID, revision uint64) (Rendered, error) {
	if !validLabel(input.Template.Name, 128) {
		return Rendered{}, ErrInvalid
	}
	if _, _, observed, err := supportedVersion(input.OcservVersion); err != nil || !observed {
		return Rendered{}, ErrInvalid
	}
	hasCapability := false
	for _, capability := range input.Capabilities {
		if capability == configprofile.PlanCapability {
			hasCapability = true
		}
	}
	if !hasCapability {
		return Rendered{}, ErrCapability
	}
	candidate := &agentv1.CompleteConfigCandidate{NodeId: nodeID[:], ExpectedRevision: revision}
	for _, item := range input.Template.Directives {
		directive := &agentv1.CompleteConfigDirective{Name: item.Name}
		if item.SecretRef == nil {
			directive.Value = &agentv1.CompleteConfigDirective_Literal{Literal: item.Value}
		} else {
			ref := item.SecretRef
			// SecretRef key is public binding metadata, not a filesystem path.
			parts := strings.Split(ref.Key, "/")
			if item.Value != "" || ref.Provider != configprofile.Provider || len(parts) != 4 || parts[0] != nodeID.String() {
				return Rendered{}, ErrInvalid
			}
			cert, err := hex.DecodeString(parts[1])
			if err != nil {
				return Rendered{}, ErrInvalid
			}
			spki, err := hex.DecodeString(parts[2])
			if err != nil {
				return Rendered{}, ErrInvalid
			}
			var ca []byte
			if parts[3] != "none" {
				ca, err = hex.DecodeString(parts[3])
				if err != nil {
					return Rendered{}, ErrInvalid
				}
			}
			if hex.EncodeToString(cert) != parts[1] || hex.EncodeToString(spki) != parts[2] || parts[3] != "none" && hex.EncodeToString(ca) != parts[3] {
				return Rendered{}, ErrInvalid
			}
			directive.Value = &agentv1.CompleteConfigDirective_Tls{Tls: &agentv1.NodeLocalTlsReference{SecretRefId: ref.ID[:], Version: ref.Version, CertificateSha256: cert, SpkiSha256: spki, CaSha256: ca}}
		}
		candidate.Directives = append(candidate.Directives, directive)
	}
	sort.Slice(candidate.Directives, func(i, j int) bool { return candidate.Directives[i].Name < candidate.Directives[j].Name })
	canonical, err := configprofile.Canonical(candidate)
	if err != nil {
		return Rendered{}, ErrInvalid
	}
	hash, _ := configprofile.Hash(candidate)
	redacted, _ := configprofile.Redacted(candidate)
	return Rendered{Candidate: canonical, CompleteCandidate: candidate, Redacted: redacted, Hash: hash, Warnings: []string{}, RequiredCapabilities: []string{configprofile.PlanCapability}}, nil
}

func supportedVersion(value string) (major, minor int, observed bool, err error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "unknown" {
		return 0, 0, false, nil
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return 0, 0, true, ErrInvalid
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, true, ErrInvalid
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, true, ErrInvalid
	}
	// Mirror the Agent adapter's admission: 1.2.x through 1.4.x, plus 1.5.0 and 1.6.0.
	patch, patchErr := strconv.Atoi(parts[2])
	previouslySupported := major == 1 && minor >= 2 && minor <= 5 && (minor != 5 || patch == 0)
	if patchErr != nil || !(previouslySupported || value == "1.6.0") {
		return 0, 0, true, fmt.Errorf("%w: unsupported Ocserv version", ErrInvalid)
	}
	return major, minor, true, nil
}

func validLabel(value string, max int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= max && labelPattern.MatchString(value)
}
