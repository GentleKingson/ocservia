package phase

import (
	"errors"
	"fmt"
	"time"
)

type Definition struct {
	Name               string
	Sequence           int
	Timeout            time.Duration
	RequiresConsumed   []string
	RequiresManifested []string
}

type Segment struct {
	Name   string
	Phases []string
}

type Graph struct {
	Profile  string
	Domain   string
	Phases   []Definition
	Segments []Segment
}

var graphs = map[string]Graph{
	"fd-a": {
		Profile: "formal",
		Domain:  "fd-a",
		Phases: []Definition{
			{Name: "prepare", Sequence: 10, Timeout: 10 * time.Minute},
			{Name: "import-peer-tunnel-nodes", Sequence: 20, Timeout: 2 * time.Minute, RequiresConsumed: []string{"tunnel-fd-b"}, RequiresManifested: []string{"tunnel-fd-a"}},
			{Name: "build-images", Sequence: 30, Timeout: 35 * time.Minute},
			{Name: "tunnel-up", Sequence: 40, Timeout: 5 * time.Minute},
			{Name: "publish-shared-secrets", Sequence: 50, Timeout: 2 * time.Minute, RequiresConsumed: []string{"shared-recipient-key"}},
			{Name: "primary-up", Sequence: 60, Timeout: 15 * time.Minute, RequiresManifested: []string{"shared-trust-ready"}},
			{Name: "agents-enroll", Sequence: 70, Timeout: 25 * time.Minute, RequiresManifested: []string{"primary-ready"}},
			{Name: "transport-trust-reload", Sequence: 80, Timeout: 10 * time.Minute, RequiresConsumed: []string{"fd-b-agents-enrolled"}, RequiresManifested: []string{"fd-a-agent-inventory"}},
			{Name: "agents-start", Sequence: 90, Timeout: 10 * time.Minute, RequiresManifested: []string{"transport-trust-ready"}},
			{Name: "pitr-prepare", Sequence: 100, Timeout: 15 * time.Minute, RequiresConsumed: []string{"production-load-active"}},
			{Name: "isolate", Sequence: 110, Timeout: 10 * time.Minute},
			{Name: "dual-primary-probes", Sequence: 120, Timeout: 10 * time.Minute, RequiresConsumed: []string{"promotion-complete"}, RequiresManifested: []string{"primary-isolated"}},
			{Name: "pitr-restore", Sequence: 130, Timeout: 15 * time.Minute},
			{Name: "rejoin", Sequence: 140, Timeout: 15 * time.Minute},
			{Name: "relay-rejoin-ready", Sequence: 150, Timeout: 2 * time.Minute},
			{Name: "relay-a-stop", Sequence: 160, Timeout: 5 * time.Minute, RequiresConsumed: []string{"relay-pre-fault-observed"}, RequiresManifested: []string{"relay-rejoin-ready"}},
			{Name: "ready", Sequence: 170, Timeout: 2 * time.Minute},
			{Name: "window-barrier-arm", Sequence: 180, Timeout: 3 * time.Minute, RequiresConsumed: []string{"window-barrier-arm-request"}, RequiresManifested: []string{"fd-a-scenarios-ready"}},
			{Name: "window-barrier-release-after-proof", Sequence: 190, Timeout: 3 * time.Minute, RequiresManifested: []string{"window-barrier-armed-fd-a"}},
			{Name: "evidence", Sequence: 200, Timeout: 15 * time.Minute, RequiresConsumed: []string{"final-freeze-request"}},
		},
		Segments: []Segment{
			{Name: "prepare", Phases: []string{"prepare"}},
			{Name: "bootstrap", Phases: []string{"import-peer-tunnel-nodes", "build-images", "tunnel-up"}},
			{Name: "shared-trust", Phases: []string{"publish-shared-secrets"}},
			{Name: "primary", Phases: []string{"primary-up"}},
			{Name: "enroll", Phases: []string{"agents-enroll"}},
			{Name: "transport-trust", Phases: []string{"transport-trust-reload"}},
			{Name: "activate-agents", Phases: []string{"agents-start"}},
			{Name: "failover-cut", Phases: []string{"pitr-prepare", "isolate"}},
			{Name: "recovery", Phases: []string{"dual-primary-probes", "pitr-restore", "rejoin", "relay-rejoin-ready"}},
			{Name: "relay-cut", Phases: []string{"relay-a-stop", "ready"}},
			{Name: "barrier-arm", Phases: []string{"window-barrier-arm"}},
			{Name: "barrier-release", Phases: []string{"window-barrier-release-after-proof"}},
			{Name: "evidence", Phases: []string{"evidence"}},
		},
	},
	"fd-b": {
		Profile: "formal",
		Domain:  "fd-b",
		Phases: []Definition{
			{Name: "prepare", Sequence: 10, Timeout: 10 * time.Minute},
			{Name: "import-peer-tunnel-nodes", Sequence: 20, Timeout: 2 * time.Minute, RequiresConsumed: []string{"tunnel-fd-a"}, RequiresManifested: []string{"tunnel-fd-b"}},
			{Name: "build-images", Sequence: 30, Timeout: 35 * time.Minute},
			{Name: "publish-shared-recipient-key", Sequence: 35, Timeout: 2 * time.Minute},
			{Name: "materialize-runtime", Sequence: 40, Timeout: 3 * time.Minute, RequiresConsumed: []string{"shared-trust-ready"}},
			{Name: "relay-up", Sequence: 50, Timeout: 5 * time.Minute},
			{Name: "tunnel-up", Sequence: 60, Timeout: 5 * time.Minute},
			{Name: "standby-bootstrap", Sequence: 70, Timeout: 15 * time.Minute, RequiresConsumed: []string{"primary-ready"}},
			{Name: "agents-enroll", Sequence: 80, Timeout: 25 * time.Minute, RequiresConsumed: []string{"fd-a-agent-inventory"}},
			{Name: "agents-start", Sequence: 90, Timeout: 10 * time.Minute, RequiresConsumed: []string{"transport-trust-ready"}, RequiresManifested: []string{"fd-b-agents-enrolled"}},
			{Name: "load-start", Sequence: 100, Timeout: 10 * time.Minute},
			{Name: "promote", Sequence: 110, Timeout: 8 * time.Minute, RequiresConsumed: []string{"primary-isolated"}, RequiresManifested: []string{"production-load-active"}},
			{Name: "relay-pre-fault", Sequence: 120, Timeout: 8 * time.Minute, RequiresConsumed: []string{"relay-rejoin-ready"}, RequiresManifested: []string{"promotion-complete"}},
			{Name: "merge-peer-evidence", Sequence: 130, Timeout: 5 * time.Minute, RequiresConsumed: []string{"fd-a-scenarios-ready"}, RequiresManifested: []string{"relay-pre-fault-observed"}},
			{Name: "scenario-relay", Sequence: 140, Timeout: 10 * time.Minute},
			{Name: "scenario-scheduler", Sequence: 150, Timeout: 10 * time.Minute},
			{Name: "scenario-owner", Sequence: 160, Timeout: 10 * time.Minute},
			{Name: "scenario-path", Sequence: 170, Timeout: 10 * time.Minute},
			{Name: "outbox-claim-before-send", Sequence: 180, Timeout: 10 * time.Minute},
			{Name: "outbox-send-before-mark", Sequence: 190, Timeout: 10 * time.Minute},
			{Name: "outbox-result-before-commit", Sequence: 200, Timeout: 10 * time.Minute},
			{Name: "window-barrier-arm", Sequence: 210, Timeout: 3 * time.Minute},
			{Name: "resource-preflight", Sequence: 220, Timeout: 2 * time.Minute, RequiresManifested: []string{"window-barrier-arm-request"}},
			{Name: "window", Sequence: 230, Timeout: 11 * time.Minute, RequiresConsumed: []string{"window-barrier-armed-fd-a"}},
			{Name: "evidence-collect", Sequence: 240, Timeout: 10 * time.Minute},
			{Name: "final-freeze", Sequence: 250, Timeout: 5 * time.Minute},
		},
		Segments: []Segment{
			{Name: "prepare", Phases: []string{"prepare"}},
			{Name: "bootstrap", Phases: []string{"import-peer-tunnel-nodes", "build-images", "publish-shared-recipient-key"}},
			{Name: "peer-runtime", Phases: []string{"materialize-runtime", "relay-up", "tunnel-up"}},
			{Name: "standby", Phases: []string{"standby-bootstrap"}},
			{Name: "enroll", Phases: []string{"agents-enroll"}},
			{Name: "load", Phases: []string{"agents-start", "load-start"}},
			{Name: "promote", Phases: []string{"promote"}},
			{Name: "relay-observe", Phases: []string{"relay-pre-fault"}},
			{Name: "fault-scenarios", Phases: []string{"merge-peer-evidence", "scenario-relay", "scenario-scheduler", "scenario-owner", "scenario-path", "outbox-claim-before-send", "outbox-send-before-mark", "outbox-result-before-commit", "window-barrier-arm"}},
			{Name: "resource-preflight", Phases: []string{"resource-preflight"}},
			{Name: "window", Phases: []string{"window"}},
			{Name: "evidence", Phases: []string{"evidence-collect", "final-freeze"}},
		},
	},
}

var smokeGraphs = map[string]Graph{
	"fd-a": {
		Profile: "smoke", Domain: "fd-a",
		Phases: []Definition{
			{Name: "prepare", Sequence: 10, Timeout: 10 * time.Minute},
			{Name: "import-peer-tunnel-nodes", Sequence: 20, Timeout: 2 * time.Minute, RequiresConsumed: []string{"smoke-tunnel-fd-b"}, RequiresManifested: []string{"smoke-tunnel-fd-a"}},
			{Name: "build-images", Sequence: 30, Timeout: 10 * time.Minute},
			{Name: "tunnel-up", Sequence: 40, Timeout: 5 * time.Minute},
			{Name: "publish-shared-secrets", Sequence: 50, Timeout: 2 * time.Minute, RequiresConsumed: []string{"smoke-shared-recipient-key"}},
			{Name: "primary-up", Sequence: 60, Timeout: 15 * time.Minute, RequiresManifested: []string{"smoke-shared-trust-ready"}},
			{Name: "agents-enroll", Sequence: 70, Timeout: 15 * time.Minute, RequiresManifested: []string{"smoke-primary-ready"}},
			{Name: "transport-trust-reload", Sequence: 80, Timeout: 10 * time.Minute, RequiresConsumed: []string{"smoke-fd-b-agents-enrolled"}, RequiresManifested: []string{"smoke-fd-a-agent-inventory"}},
			{Name: "agents-start", Sequence: 90, Timeout: 10 * time.Minute, RequiresManifested: []string{"smoke-transport-trust-ready"}},
			{Name: "smoke-isolate", Sequence: 100, Timeout: 10 * time.Minute, RequiresConsumed: []string{"smoke-session"}},
			{Name: "smoke-evidence", Sequence: 110, Timeout: 10 * time.Minute, RequiresConsumed: []string{"smoke-promotion-complete"}, RequiresManifested: []string{"smoke-primary-isolated"}},
		},
		Segments: []Segment{{Name: "prepare", Phases: []string{"prepare"}}, {Name: "bootstrap", Phases: []string{"import-peer-tunnel-nodes", "build-images", "tunnel-up"}}, {Name: "shared-trust", Phases: []string{"publish-shared-secrets"}}, {Name: "primary", Phases: []string{"primary-up"}}, {Name: "enroll", Phases: []string{"agents-enroll"}}, {Name: "transport-trust", Phases: []string{"transport-trust-reload"}}, {Name: "activate-agents", Phases: []string{"agents-start"}}, {Name: "isolate", Phases: []string{"smoke-isolate"}}, {Name: "evidence", Phases: []string{"smoke-evidence"}}},
	},
	"fd-b": {
		Profile: "smoke", Domain: "fd-b",
		Phases: []Definition{
			{Name: "prepare", Sequence: 10, Timeout: 10 * time.Minute},
			{Name: "import-peer-tunnel-nodes", Sequence: 20, Timeout: 2 * time.Minute, RequiresConsumed: []string{"smoke-tunnel-fd-a"}, RequiresManifested: []string{"smoke-tunnel-fd-b"}},
			{Name: "build-images", Sequence: 30, Timeout: 10 * time.Minute},
			{Name: "publish-shared-recipient-key", Sequence: 35, Timeout: 2 * time.Minute},
			{Name: "materialize-runtime", Sequence: 40, Timeout: 3 * time.Minute, RequiresConsumed: []string{"smoke-shared-trust-ready"}},
			{Name: "relay-up", Sequence: 50, Timeout: 5 * time.Minute},
			{Name: "tunnel-up", Sequence: 60, Timeout: 5 * time.Minute},
			{Name: "standby-bootstrap", Sequence: 70, Timeout: 15 * time.Minute, RequiresConsumed: []string{"smoke-primary-ready"}},
			{Name: "agents-enroll", Sequence: 80, Timeout: 15 * time.Minute, RequiresConsumed: []string{"smoke-fd-a-agent-inventory"}},
			{Name: "agents-start", Sequence: 90, Timeout: 10 * time.Minute, RequiresConsumed: []string{"smoke-transport-trust-ready"}, RequiresManifested: []string{"smoke-fd-b-agents-enrolled"}},
			{Name: "smoke-session", Sequence: 100, Timeout: 10 * time.Minute},
			{Name: "promote", Sequence: 110, Timeout: 8 * time.Minute, RequiresConsumed: []string{"smoke-primary-isolated"}, RequiresManifested: []string{"smoke-session"}},
			{Name: "smoke-evidence", Sequence: 120, Timeout: 10 * time.Minute, RequiresManifested: []string{"smoke-promotion-complete"}},
		},
		Segments: []Segment{{Name: "prepare", Phases: []string{"prepare"}}, {Name: "bootstrap", Phases: []string{"import-peer-tunnel-nodes", "build-images", "publish-shared-recipient-key"}}, {Name: "peer-runtime", Phases: []string{"materialize-runtime", "relay-up", "tunnel-up"}}, {Name: "standby", Phases: []string{"standby-bootstrap"}}, {Name: "enroll", Phases: []string{"agents-enroll"}}, {Name: "activate-agents", Phases: []string{"agents-start"}}, {Name: "session", Phases: []string{"smoke-session"}}, {Name: "promote", Phases: []string{"promote"}}, {Name: "evidence", Phases: []string{"smoke-evidence"}}},
	},
}

var manifestedByCheckpoint = map[string]struct {
	Domain string
	Phase  string
}{
	"tunnel-fd-a":                {Domain: "fd-a", Phase: "prepare"},
	"tunnel-fd-b":                {Domain: "fd-b", Phase: "prepare"},
	"shared-recipient-key":       {Domain: "fd-b", Phase: "publish-shared-recipient-key"},
	"shared-trust-ready":         {Domain: "fd-a", Phase: "publish-shared-secrets"},
	"primary-ready":              {Domain: "fd-a", Phase: "primary-up"},
	"fd-a-agent-inventory":       {Domain: "fd-a", Phase: "agents-enroll"},
	"fd-b-agents-enrolled":       {Domain: "fd-b", Phase: "agents-enroll"},
	"transport-trust-ready":      {Domain: "fd-a", Phase: "transport-trust-reload"},
	"primary-isolated":           {Domain: "fd-a", Phase: "isolate"},
	"production-load-active":     {Domain: "fd-b", Phase: "load-start"},
	"promotion-complete":         {Domain: "fd-b", Phase: "promote"},
	"relay-rejoin-ready":         {Domain: "fd-a", Phase: "relay-rejoin-ready"},
	"relay-pre-fault-observed":   {Domain: "fd-b", Phase: "relay-pre-fault"},
	"fd-a-scenarios-ready":       {Domain: "fd-a", Phase: "ready"},
	"window-barrier-arm-request": {Domain: "fd-b", Phase: "window-barrier-arm"},
	"window-barrier-armed-fd-a":  {Domain: "fd-a", Phase: "window-barrier-arm"},
	"final-freeze-request":       {Domain: "fd-b", Phase: "final-freeze"},
}

var smokeManifestedByCheckpoint = map[string]struct{ Domain, Phase string }{
	"smoke-tunnel-fd-a": {"fd-a", "prepare"}, "smoke-tunnel-fd-b": {"fd-b", "prepare"},
	"smoke-shared-recipient-key": {"fd-b", "publish-shared-recipient-key"},
	"smoke-shared-trust-ready":   {"fd-a", "publish-shared-secrets"}, "smoke-primary-ready": {"fd-a", "primary-up"},
	"smoke-fd-a-agent-inventory": {"fd-a", "agents-enroll"}, "smoke-fd-b-agents-enrolled": {"fd-b", "agents-enroll"},
	"smoke-transport-trust-ready": {"fd-a", "transport-trust-reload"}, "smoke-session": {"fd-b", "smoke-session"},
	"smoke-primary-isolated": {"fd-a", "smoke-isolate"}, "smoke-promotion-complete": {"fd-b", "promote"},
}

func ResolveProfileGraph(profile, domain string) (Graph, error) {
	if profile == "" || profile == "formal" {
		return ResolveGraph(domain)
	}
	if profile != "smoke" {
		return Graph{}, fmt.Errorf("unsupported harness profile %q", profile)
	}
	graph, ok := smokeGraphs[domain]
	if !ok {
		return Graph{}, fmt.Errorf("unsupported failure domain %q", domain)
	}
	return graph, nil
}

func ResolveProfileSegment(profile, domain, name string) (Graph, Segment, error) {
	graph, err := ResolveProfileGraph(profile, domain)
	if err != nil {
		return Graph{}, Segment{}, err
	}
	for _, segment := range graph.Segments {
		if segment.Name == name {
			return graph, segment, nil
		}
	}
	return Graph{}, Segment{}, fmt.Errorf("unknown %s %s segment %q", graph.Profile, domain, name)
}

func ResolveGraph(domain string) (Graph, error) {
	graph, ok := graphs[domain]
	if !ok {
		return Graph{}, fmt.Errorf("unsupported failure domain %q", domain)
	}
	return graph, nil
}

func ResolveSegment(domain, name string) (Graph, Segment, error) {
	graph, err := ResolveGraph(domain)
	if err != nil {
		return Graph{}, Segment{}, err
	}
	for _, segment := range graph.Segments {
		if segment.Name == name {
			return graph, segment, nil
		}
	}
	return Graph{}, Segment{}, fmt.Errorf("unknown %s segment %q", domain, name)
}

func (graph Graph) Definition(name string) (Definition, error) {
	for _, definition := range graph.Phases {
		if definition.Name == name {
			return definition, nil
		}
	}
	return Definition{}, fmt.Errorf("unknown %s phase %q", graph.Domain, name)
}

func (graph Graph) ExpectedAfter(sequence int) (Definition, error) {
	for _, definition := range graph.Phases {
		if definition.Sequence > sequence {
			return definition, nil
		}
	}
	return Definition{}, errors.New("runtime phase graph is already complete")
}

func RequiredManifestPhase(domain, checkpoint string) (string, error) {
	return RequiredManifestPhaseForProfile("formal", domain, checkpoint)
}

func RequiredManifestPhaseForProfile(profile, domain, checkpoint string) (string, error) {
	manifested := manifestedByCheckpoint
	if profile == "smoke" {
		manifested = smokeManifestedByCheckpoint
	} else if profile != "" && profile != "formal" {
		return "", fmt.Errorf("unsupported harness profile %q", profile)
	}
	requirement, ok := manifested[checkpoint]
	if !ok || requirement.Domain != domain {
		return "", fmt.Errorf("checkpoint %q is not produced by %s", checkpoint, domain)
	}
	return requirement.Phase, nil
}
