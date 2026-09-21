package configplan

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/configprofile"
	"github.com/google/uuid"
)

func completeRenderFixture() (RenderInput, uuid.UUID) {
	node := uuid.MustParse("01900000-0000-7000-8000-000000000001")
	ref := &SecretRef{ID: uuid.MustParse("01900000-0000-7000-8000-000000000002"), Provider: configprofile.Provider, Version: "v1", Key: node.String() + "/" + strings.Repeat("42", 32) + "/" + strings.Repeat("43", 32) + "/none"}
	return RenderInput{Template: Template{Name: "complete", Directives: []Directive{
		{Name: "auth", Value: "plain[passwd=/etc/ocserv/ocpasswd]"}, {Name: "cookie-timeout", Value: "300"},
		{Name: "device", Value: "vpns"}, {Name: "dns", Value: "1.1.1.1"}, {Name: "ipv4-network", Value: "10.42.0.0/24"},
		{Name: "max-clients", Value: "128"}, {Name: "max-same-clients", Value: "2"},
		{Name: "server-cert", SecretRef: ref}, {Name: "server-key", SecretRef: ref},
		{Name: "socket-file", Value: "/run/ocserv.socket"}, {Name: "tcp-port", Value: "443"}, {Name: "udp-port", Value: "0"},
	}}, OcservVersion: "1.2.4", Capabilities: []string{configprofile.PlanCapability}}, node
}

func TestCompleteRenderBindsNodeRevisionAndResolvedPublicReferences(t *testing.T) {
	input, node := completeRenderFixture()
	first, err := renderComplete(input, node, 7)
	if err != nil {
		t.Fatal(err)
	}
	input.Template.Directives[0], input.Template.Directives[1] = input.Template.Directives[1], input.Template.Directives[0]
	second, err := renderComplete(input, node, 7)
	if err != nil || !bytes.Equal(first.Candidate, second.Candidate) || first.Hash != second.Hash {
		t.Fatal("noncanonical rendering", err)
	}
	if first.CompleteCandidate == nil || len(first.Warnings) != 0 || len(first.RequiredCapabilities) != 1 || first.RequiredCapabilities[0] != configprofile.PlanCapability {
		t.Fatal("incomplete profile contract", first)
	}
	if strings.Contains(first.Redacted, input.Template.Directives[7].SecretRef.Key) || strings.Contains(first.Redacted, "/config-tls/") {
		t.Fatal("public binding/path leaked into redacted diff")
	}
	if !strings.Contains(first.Redacted, "run-as-user = ocservia-vpn") {
		t.Fatal("fixed unprivileged worker omitted")
	}
	changed, err := renderComplete(input, node, 8)
	if err != nil || changed.Hash == first.Hash {
		t.Fatal("revision not bound")
	}
	if _, err := renderComplete(input, uuid.Must(uuid.NewV7()), 7); !errors.Is(err, ErrInvalid) {
		t.Fatal("cross-node TLS accepted", err)
	}
	input.Template.Directives[7].SecretRef.Version = "v2"
	changed, err = renderComplete(input, node, 7)
	if err != nil || changed.Hash == first.Hash {
		t.Fatal("TLS version not bound")
	}
}

func TestCompleteRenderRejectsOldNodesAndIncompleteOrUnsafeCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*RenderInput)
	}{
		{"legacy capabilities", func(i *RenderInput) { i.Capabilities = []string{"ocserv.config.plan", "config.tls"} }},
		{"unobserved version", func(i *RenderInput) { i.OcservVersion = "unknown" }},
		{"unsupported version", func(i *RenderInput) { i.OcservVersion = "2.0.0" }},
		{"missing device", func(i *RenderInput) {
			i.Template.Directives = append(i.Template.Directives[:2], i.Template.Directives[3:]...)
		}},
		{"duplicate", func(i *RenderInput) { i.Template.Directives = append(i.Template.Directives, i.Template.Directives[0]) }},
		{"arbitrary auth path", func(i *RenderInput) { i.Template.Directives[0].Value = "plain[passwd=/tmp/passwd]" }},
		{"variable expansion", func(i *RenderInput) { i.NodeVariables = map[string]string{"PORT": "443"} }},
		{"unresolved TLS", func(i *RenderInput) { i.Template.Directives[7].SecretRef.Provider = "node" }},
		{"path in public binding", func(i *RenderInput) { i.Template.Directives[7].SecretRef.Key = "/etc/ocserv/key" }},
		{"unsafe TLS version", func(i *RenderInput) { i.Template.Directives[7].SecretRef.Version = "../key" }},
		{"unsafe device", func(i *RenderInput) { i.Template.Directives[2].Value = "vpns\nrun-as-user=root" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, node := completeRenderFixture()
			tc.change(&input)
			if _, err := renderComplete(input, node, 0); err == nil {
				t.Fatal("unsafe complete profile accepted")
			}
		})
	}
}
