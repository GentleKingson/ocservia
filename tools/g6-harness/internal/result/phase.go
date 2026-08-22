package result

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".g6-phase-result-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(phase); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
