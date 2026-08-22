package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/phase"
)

const (
	SchemaVersion      = "ocservia.g6-runtime-state.v1"
	EventSchemaVersion = "ocservia.g6-runtime-event.v1"
)

type Binding struct {
	CandidateSHA  string `json:"candidate_sha"`
	RunID         string `json:"run_id"`
	RunAttempt    int    `json:"run_attempt"`
	EnvironmentID string `json:"environment_id"`
	Authority     string `json:"authority"`
}

type PhaseRecord struct {
	Name        string    `json:"name"`
	Sequence    int       `json:"sequence"`
	CompletedAt time.Time `json:"completed_at"`
}

type ActivePhase struct {
	Name      string    `json:"name"`
	Sequence  int       `json:"sequence"`
	StartedAt time.Time `json:"started_at"`
}

type State struct {
	SchemaVersion         string        `json:"schema_version"`
	Domain                string        `json:"domain"`
	Binding               Binding       `json:"binding"`
	Status                string        `json:"status"`
	CurrentSequence       int           `json:"current_sequence"`
	CompletedPhases       []PhaseRecord `json:"completed_phases"`
	ManifestedCheckpoints []string      `json:"manifested_checkpoints"`
	ConsumedCheckpoints   []string      `json:"consumed_checkpoints"`
	ActivePhase           *ActivePhase  `json:"active_phase,omitempty"`
	Failure               string        `json:"failure,omitempty"`
	UpdatedAt             time.Time     `json:"updated_at"`
}

type Event struct {
	SchemaVersion string    `json:"schema_version"`
	Domain        string    `json:"domain"`
	Binding       Binding   `json:"binding"`
	Type          string    `json:"type"`
	Phase         string    `json:"phase,omitempty"`
	Checkpoint    string    `json:"checkpoint,omitempty"`
	Sequence      int       `json:"sequence,omitempty"`
	At            time.Time `json:"at"`
	Message       string    `json:"message,omitempty"`
}

type Store struct {
	Root    string
	Graph   phase.Graph
	Binding Binding
	Domain  string
	Now     func() time.Time
	lock    *os.File
	state   State
}

func Open(root, domain string, binding Binding, graph phase.Graph, now func() time.Time) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("runtime state root must be absolute")
	}
	if graph.Domain != domain {
		return nil, errors.New("runtime graph does not match the failure domain")
	}
	if now == nil {
		return nil, errors.New("runtime state clock is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, "state.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire runtime state lock: %w", err)
	}
	store := &Store{Root: root, Graph: graph, Binding: binding, Domain: domain, Now: now, lock: lock}
	if err := store.loadOrInitialize(); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

func (store *Store) Close() error {
	if store.lock == nil {
		return nil
	}
	name := store.lock.Name()
	closeErr := store.lock.Close()
	removeErr := os.Remove(name)
	store.lock = nil
	return errors.Join(closeErr, removeErr)
}

func (store *Store) Snapshot() State { return store.state }

func (store *Store) Begin(definition phase.Definition) (time.Time, error) {
	if store.state.Status != "running" {
		return time.Time{}, fmt.Errorf("runtime state is %s", store.state.Status)
	}
	if store.state.ActivePhase != nil {
		return time.Time{}, fmt.Errorf("phase %s is already active", store.state.ActivePhase.Name)
	}
	expected, err := store.Graph.ExpectedAfter(store.state.CurrentSequence)
	if err != nil {
		return time.Time{}, err
	}
	if expected.Name != definition.Name || expected.Sequence != definition.Sequence {
		return time.Time{}, fmt.Errorf("phase %s/%d is out of order; expected %s/%d", definition.Name, definition.Sequence, expected.Name, expected.Sequence)
	}
	for _, checkpoint := range definition.RequiresConsumed {
		if !slices.Contains(store.state.ConsumedCheckpoints, checkpoint) {
			return time.Time{}, fmt.Errorf("phase %s requires consumed checkpoint %s", definition.Name, checkpoint)
		}
	}
	for _, checkpoint := range definition.RequiresManifested {
		if !slices.Contains(store.state.ManifestedCheckpoints, checkpoint) {
			return time.Time{}, fmt.Errorf("phase %s requires manifested checkpoint %s", definition.Name, checkpoint)
		}
	}
	started := store.Now().UTC()
	store.state.ActivePhase = &ActivePhase{Name: definition.Name, Sequence: definition.Sequence, StartedAt: started}
	store.state.UpdatedAt = started
	if err := store.persist(); err != nil {
		return time.Time{}, err
	}
	return started, store.appendEvent(Event{Type: "phase_started", Phase: definition.Name, Sequence: definition.Sequence, At: started})
}

func (store *Store) Complete(definition phase.Definition) error {
	if store.state.ActivePhase == nil || store.state.ActivePhase.Name != definition.Name || store.state.ActivePhase.Sequence != definition.Sequence {
		return fmt.Errorf("phase %s is not the active phase", definition.Name)
	}
	completed := store.Now().UTC()
	store.state.CompletedPhases = append(store.state.CompletedPhases, PhaseRecord{Name: definition.Name, Sequence: definition.Sequence, CompletedAt: completed})
	store.state.CurrentSequence = definition.Sequence
	store.state.ActivePhase = nil
	store.state.UpdatedAt = completed
	if definition.Sequence == store.Graph.Phases[len(store.Graph.Phases)-1].Sequence {
		store.state.Status = "completed"
	}
	if err := store.persist(); err != nil {
		return err
	}
	return store.appendEvent(Event{Type: "phase_completed", Phase: definition.Name, Sequence: definition.Sequence, At: completed})
}

func (store *Store) Fail(definition phase.Definition, message string) error {
	failed := store.Now().UTC()
	store.state.Status = "failed"
	store.state.Failure = message
	store.state.ActivePhase = nil
	store.state.UpdatedAt = failed
	if err := store.persist(); err != nil {
		return err
	}
	return store.appendEvent(Event{Type: "phase_failed", Phase: definition.Name, Sequence: definition.Sequence, At: failed, Message: message})
}

func (store *Store) RecordManifested(checkpoint, requiredPhase string) error {
	if store.state.Status == "failed" {
		return errors.New("failed runtime state cannot manifest checkpoints")
	}
	if slices.Contains(store.state.ManifestedCheckpoints, checkpoint) {
		return fmt.Errorf("checkpoint %s was already manifested", checkpoint)
	}
	if !store.phaseCompleted(requiredPhase) {
		return fmt.Errorf("checkpoint %s requires completed phase %s", checkpoint, requiredPhase)
	}
	now := store.Now().UTC()
	store.state.ManifestedCheckpoints = append(store.state.ManifestedCheckpoints, checkpoint)
	store.state.UpdatedAt = now
	if err := store.persist(); err != nil {
		return err
	}
	return store.appendEvent(Event{Type: "checkpoint_manifested", Checkpoint: checkpoint, At: now})
}

func (store *Store) RecordConsumed(checkpoint string) error {
	if store.state.Status != "running" {
		return fmt.Errorf("runtime state is %s and cannot consume checkpoints", store.state.Status)
	}
	known := false
	for _, definition := range store.Graph.Phases {
		if slices.Contains(definition.RequiresConsumed, checkpoint) {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("checkpoint %s is not consumed by the typed graph", checkpoint)
	}
	if slices.Contains(store.state.ConsumedCheckpoints, checkpoint) {
		return fmt.Errorf("checkpoint %s was already consumed by the runtime", checkpoint)
	}
	now := store.Now().UTC()
	store.state.ConsumedCheckpoints = append(store.state.ConsumedCheckpoints, checkpoint)
	store.state.UpdatedAt = now
	if err := store.persist(); err != nil {
		return err
	}
	return store.appendEvent(Event{Type: "checkpoint_consumed", Checkpoint: checkpoint, At: now})
}

func (store *Store) phaseCompleted(name string) bool {
	return slices.ContainsFunc(store.state.CompletedPhases, func(record PhaseRecord) bool { return record.Name == name })
}

func (store *Store) loadOrInitialize() error {
	path := filepath.Join(store.Root, "state.json")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		now := store.Now().UTC()
		store.state = State{SchemaVersion: SchemaVersion, Domain: store.Domain, Binding: store.Binding, Status: "running", UpdatedAt: now}
		if err := store.persist(); err != nil {
			return err
		}
		return store.appendEvent(Event{Type: "runtime_created", At: now})
	}
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&store.state); err != nil {
		return fmt.Errorf("decode runtime state: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("runtime state contains trailing JSON")
	}
	if store.state.SchemaVersion != SchemaVersion || store.state.Domain != store.Domain || store.state.Binding != store.Binding {
		return errors.New("runtime state belongs to another schema, domain, candidate, run, attempt, environment, or authority")
	}
	if err := store.validateLoadedState(); err != nil {
		return fmt.Errorf("validate runtime state: %w", err)
	}
	if store.state.ActivePhase != nil {
		return fmt.Errorf("runtime state contains interrupted active phase %s", store.state.ActivePhase.Name)
	}
	return nil
}

func (store *Store) validateLoadedState() error {
	if store.state.Status != "running" && store.state.Status != "completed" && store.state.Status != "failed" {
		return fmt.Errorf("unsupported status %q", store.state.Status)
	}
	if len(store.state.CompletedPhases) > len(store.Graph.Phases) {
		return errors.New("completed phase list exceeds the typed graph")
	}
	current := 0
	for index, record := range store.state.CompletedPhases {
		expected := store.Graph.Phases[index]
		if record.Name != expected.Name || record.Sequence != expected.Sequence || record.CompletedAt.IsZero() {
			return errors.New("completed phases are not an exact graph prefix")
		}
		current = record.Sequence
	}
	if store.state.CurrentSequence != current {
		return errors.New("current sequence does not match completed phases")
	}
	graphComplete := len(store.state.CompletedPhases) == len(store.Graph.Phases)
	if (store.state.Status == "completed") != graphComplete {
		return errors.New("completed status does not match the typed graph")
	}
	if store.state.Status == "failed" && store.state.Failure == "" {
		return errors.New("failed state lacks a failure reason")
	}
	if store.state.Status != "failed" && store.state.Failure != "" {
		return errors.New("non-failed state contains a failure reason")
	}
	if err := validateUnique("manifested", store.state.ManifestedCheckpoints); err != nil {
		return err
	}
	for _, checkpoint := range store.state.ManifestedCheckpoints {
		required, err := phase.RequiredManifestPhase(store.Domain, checkpoint)
		if err != nil || !store.phaseCompleted(required) {
			return fmt.Errorf("manifested checkpoint %s lacks its exact producer phase", checkpoint)
		}
	}
	if err := validateUnique("consumed", store.state.ConsumedCheckpoints); err != nil {
		return err
	}
	allowedConsumed := make(map[string]bool)
	for _, definition := range store.Graph.Phases {
		for _, checkpoint := range definition.RequiresConsumed {
			allowedConsumed[checkpoint] = true
		}
	}
	for _, checkpoint := range store.state.ConsumedCheckpoints {
		if !allowedConsumed[checkpoint] {
			return fmt.Errorf("consumed checkpoint %s is not part of the typed graph", checkpoint)
		}
	}
	return nil
}

func validateUnique(kind string, values []string) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return fmt.Errorf("%s checkpoint %s is duplicated", kind, value)
		}
		seen[value] = true
	}
	return nil
}

func (store *Store) persist() error {
	return writeJSONAtomic(filepath.Join(store.Root, "state.json"), store.state)
}

func (store *Store) appendEvent(event Event) error {
	event.SchemaVersion = EventSchemaVersion
	event.Domain = store.Domain
	event.Binding = store.Binding
	file, err := os.OpenFile(filepath.Join(store.Root, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encodeErr := encoder.Encode(event)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(encodeErr, syncErr, closeErr)
}

func writeJSONAtomic(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".g6-state-*")
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
	if err := encoder.Encode(value); err != nil {
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
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(directoryHandle.Sync(), directoryHandle.Close())
}
