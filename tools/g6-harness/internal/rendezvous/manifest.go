package rendezvous

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ManifestFilename = "checkpoint-manifest.json"

type Payload struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	SchemaVersion  string    `json:"schema_version"`
	ArtifactName   string    `json:"artifact_name"`
	Checkpoint     string    `json:"checkpoint"`
	Sequence       int       `json:"sequence"`
	ProducerDomain string    `json:"producer_domain"`
	CandidateSHA   string    `json:"candidate_sha"`
	RunID          string    `json:"run_id"`
	RunAttempt     int       `json:"run_attempt"`
	EnvironmentID  string    `json:"environment_id"`
	Authority      string    `json:"authority"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	Payloads       []Payload `json:"payloads"`
}

func CreateManifest(root, artifactName, producerDomain string, binding Binding, now time.Time, ttl time.Duration) (Manifest, error) {
	contract, err := ResolveContract(artifactName, binding)
	if err != nil {
		return Manifest{}, err
	}
	if producerDomain != contract.ProducerDomain {
		return Manifest{}, fmt.Errorf("artifact %s must be produced by %s, not %s", artifactName, contract.ProducerDomain, producerDomain)
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return Manifest{}, errors.New("checkpoint TTL must be greater than zero and no more than 24 hours")
	}
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		return Manifest{}, fmt.Errorf("checkpoint payload root must be an existing directory: %w", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(root, ManifestFilename)); statErr == nil {
		return Manifest{}, errors.New("checkpoint manifest already exists")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("inspect existing checkpoint manifest: %w", statErr)
	}
	payloads, err := inventory(root)
	if err != nil {
		return Manifest{}, err
	}
	if len(payloads) == 0 {
		return Manifest{}, errors.New("checkpoint payload is empty")
	}
	created := now.UTC()
	manifest := Manifest{
		SchemaVersion:  CheckpointSchemaVersion,
		ArtifactName:   artifactName,
		Checkpoint:     contract.Checkpoint,
		Sequence:       contract.Sequence,
		ProducerDomain: contract.ProducerDomain,
		CandidateSHA:   binding.CandidateSHA,
		RunID:          binding.RunID,
		RunAttempt:     binding.RunAttempt,
		EnvironmentID:  binding.EnvironmentID,
		Authority:      binding.Authority,
		CreatedAt:      created,
		ExpiresAt:      created.Add(ttl),
		Payloads:       payloads,
	}
	if err := writeJSONAtomic(filepath.Join(root, ManifestFilename), manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ReadAndValidateManifest(root, artifactName string, binding Binding, now time.Time) (Manifest, error) {
	contract, err := ResolveContract(artifactName, binding)
	if err != nil {
		return Manifest{}, err
	}
	file, err := os.Open(filepath.Join(root, ManifestFilename))
	if err != nil {
		return Manifest{}, fmt.Errorf("open checkpoint manifest: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Manifest{}, errors.New("checkpoint manifest contains trailing JSON")
	}
	if manifest.SchemaVersion != CheckpointSchemaVersion ||
		manifest.ArtifactName != artifactName ||
		manifest.Checkpoint != contract.Checkpoint ||
		manifest.Sequence != contract.Sequence ||
		manifest.ProducerDomain != contract.ProducerDomain {
		return Manifest{}, errors.New("checkpoint manifest does not match the artifact contract")
	}
	if manifest.CandidateSHA != binding.CandidateSHA || manifest.RunID != binding.RunID ||
		manifest.RunAttempt != binding.RunAttempt || manifest.EnvironmentID != binding.EnvironmentID ||
		manifest.Authority != binding.Authority {
		return Manifest{}, errors.New("checkpoint manifest does not match the exact candidate/run/attempt/environment binding")
	}
	if manifest.CreatedAt.IsZero() || manifest.ExpiresAt.IsZero() || !manifest.ExpiresAt.After(manifest.CreatedAt) {
		return Manifest{}, errors.New("checkpoint manifest has an invalid lifetime")
	}
	if manifest.CreatedAt.After(now.UTC().Add(5 * time.Minute)) {
		return Manifest{}, errors.New("checkpoint manifest was created in the future")
	}
	if now.UTC().After(manifest.ExpiresAt) {
		return Manifest{}, errors.New("checkpoint manifest is expired")
	}
	actual, err := inventory(root)
	if err != nil {
		return Manifest{}, err
	}
	if len(manifest.Payloads) != len(actual) {
		return Manifest{}, errors.New("checkpoint payload file set does not match its manifest")
	}
	for index := range actual {
		expected := manifest.Payloads[index]
		if expected.Path != actual[index].Path || !digestPattern.MatchString(expected.SHA256) || expected.SHA256 != actual[index].SHA256 {
			return Manifest{}, fmt.Errorf("checkpoint payload digest mismatch for %s", actual[index].Path)
		}
	}
	return manifest, nil
}

func inventory(root string) ([]Payload, error) {
	var payloads []Payload
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("checkpoint payload contains a non-regular file: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == ManifestFilename {
			return nil
		}
		if relative == "" || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
			return fmt.Errorf("unsafe checkpoint payload path %q", relative)
		}
		digest, err := digestFile(path)
		if err != nil {
			return err
		}
		payloads = append(payloads, Payload{Path: relative, SHA256: digest})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(payloads, func(i, j int) bool { return payloads[i].Path < payloads[j].Path })
	return payloads, nil
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeJSONAtomic(path string, value any) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".g6-json-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
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
	return os.Rename(temporaryName, path)
}
