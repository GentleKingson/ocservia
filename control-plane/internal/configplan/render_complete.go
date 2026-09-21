package configplan

import (
	"encoding/hex"
	"sort"
	"strings"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/google/uuid"
)

func completeProfile(template Template) bool {
	for _, directive := range template.Directives {
		if directive.SecretRef != nil && directive.SecretRef.Provider == configprofile.Provider {
			return true
		}
	}
	return false
}

func renderComplete(input RenderInput, nodeID uuid.UUID, revision uint64) (Rendered, error) {
	if !validLabel(input.Template.Name, 128) || len(input.NodeVariables) != 0 {
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
