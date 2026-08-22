package cleanup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/state"
)

const SchemaVersion = "ocservia.g6-resource-registry.v1"

type Resource struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Registry struct {
	SchemaVersion string        `json:"schema_version"`
	Domain        string        `json:"domain"`
	Binding       state.Binding `json:"binding"`
	RunID         string        `json:"domain_run_id"`
	Resources     []Resource    `json:"resources"`
	CleanupStatus string        `json:"cleanup_status"`
	CleanupAt     *time.Time    `json:"cleanup_at,omitempty"`
	CleanupError  string        `json:"cleanup_error,omitempty"`
}

func Expected(domain, domainRunID, runnerTemp string, binding state.Binding, environment map[string]string) (Registry, error) {
	if domain != "fd-a" && domain != "fd-b" {
		return Registry{}, fmt.Errorf("unsupported failure domain %q", domain)
	}
	if !filepath.IsAbs(runnerTemp) {
		return Registry{}, errors.New("runner temp must be absolute")
	}
	resources := []Resource{
		{Kind: "work_directory", ID: filepath.Join(runnerTemp, "g6-readiness-"+domainRunID)},
		{Kind: "diagnostics_directory", ID: filepath.Join(runnerTemp, "artifacts", "g6-readiness-"+domain)},
		{Kind: "compose_project", ID: "ocservia-g6-rd-" + domainRunID},
		{Kind: "docker_run_label", ID: domainRunID},
		{Kind: "docker_network", ID: "ocservia-g6-rd-" + domainRunID + "_relay-a-only"},
	}
	for _, name := range []string{"G6RD_AGENT_IMAGE", "G6RD_CONTROL_PLANE_IMAGE", "G6RD_TRANSPORTD_IMAGE", "G6RD_RELAY_IMAGE", "G6RD_PROBE_IMAGE"} {
		value := environment[name]
		if value == "" {
			return Registry{}, fmt.Errorf("%s is required for the cleanup registry", name)
		}
		resources = append(resources, Resource{Kind: "container_image", ID: value})
	}
	resources = append(resources, Resource{Kind: "container_image", ID: "postgres:17.10-bookworm"})
	return Registry{SchemaVersion: SchemaVersion, Domain: domain, Binding: binding, RunID: domainRunID, Resources: resources, CleanupStatus: "pending"}, nil
}

func Ensure(path string, expected Registry) error {
	current, err := Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return Write(path, expected)
	}
	if err != nil {
		return err
	}
	return validate(current, expected)
}

func LoadAndValidate(path string, expected Registry) (Registry, error) {
	current, err := Load(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := Write(path, expected); err != nil {
			return Registry{}, err
		}
		return expected, nil
	}
	if err != nil {
		return Registry{}, err
	}
	if err := validate(current, expected); err != nil {
		return Registry{}, err
	}
	return current, nil
}

func Load(path string) (Registry, error) {
	file, err := os.Open(path)
	if err != nil {
		return Registry{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var registry Registry
	if err := decoder.Decode(&registry); err != nil {
		return Registry{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Registry{}, errors.New("resource registry contains trailing JSON")
	}
	return registry, nil
}

func Mark(path string, registry Registry, status string, cleanupErr error, now time.Time) error {
	if status != "passed" && status != "failed" {
		return errors.New("cleanup status must be passed or failed")
	}
	completed := now.UTC()
	registry.CleanupStatus = status
	registry.CleanupAt = &completed
	if cleanupErr != nil {
		registry.CleanupError = cleanupErr.Error()
	} else {
		registry.CleanupError = ""
	}
	return Write(path, registry)
}

func Write(path string, registry Registry) error {
	if !filepath.IsAbs(path) {
		return errors.New("resource registry path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".g6-resources-*")
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
	if err := encoder.Encode(registry); err != nil {
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

func validate(current, expected Registry) error {
	if current.SchemaVersion != expected.SchemaVersion || current.Domain != expected.Domain || current.Binding != expected.Binding || current.RunID != expected.RunID {
		return errors.New("resource registry belongs to another schema, domain, candidate, run, attempt, environment, or authority")
	}
	if len(current.Resources) != len(expected.Resources) {
		return errors.New("resource registry does not match the exact run resource set")
	}
	for index := range expected.Resources {
		if current.Resources[index] != expected.Resources[index] {
			return errors.New("resource registry contains an unexpected resource")
		}
	}
	switch current.CleanupStatus {
	case "pending":
		if current.CleanupAt != nil || current.CleanupError != "" {
			return errors.New("pending resource registry contains a cleanup outcome")
		}
	case "passed":
		if current.CleanupAt == nil || current.CleanupError != "" {
			return errors.New("passed resource registry has an invalid cleanup outcome")
		}
	case "failed":
		if current.CleanupAt == nil || current.CleanupError == "" {
			return errors.New("failed resource registry has an invalid cleanup outcome")
		}
	default:
		return errors.New("resource registry has an unsupported cleanup status")
	}
	return nil
}
