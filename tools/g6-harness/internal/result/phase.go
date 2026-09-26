package result

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/atomicjson"
	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/state"
)

const PhaseSchemaVersion = "ocservia.g6-phase-result.v1"

type Failure struct {
	Class           string   `json:"class"`
	Code            string   `json:"code"`
	Message         string   `json:"message"`
	Expected        string   `json:"expected,omitempty"`
	Actual          string   `json:"actual,omitempty"`
	DiagnosticPaths []string `json:"diagnostic_paths"`
}

type Phase struct {
	SchemaVersion string        `json:"schema_version"`
	Domain        string        `json:"domain"`
	Binding       state.Binding `json:"binding"`
	Segment       string        `json:"segment"`
	Phase         string        `json:"phase"`
	Sequence      int           `json:"sequence"`
	Status        string        `json:"status"`
	StartedAt     time.Time     `json:"started_at"`
	CompletedAt   time.Time     `json:"completed_at"`
	Deadline      time.Time     `json:"deadline"`
	ExitCode      *int          `json:"exit_code,omitempty"`
	Failure       *Failure      `json:"failure,omitempty"`
}

func Write(path string, phase Phase) error {
	if !filepath.IsAbs(path) {
		return errors.New("phase result path must be absolute")
	}
	phase.SchemaVersion = PhaseSchemaVersion
	return atomicjson.Write(path, phase)
}
