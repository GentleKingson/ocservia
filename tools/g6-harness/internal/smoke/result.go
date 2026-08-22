package smoke

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/rendezvous"
)

const (
	DomainSchemaVersion       = "ocservia.g6-harness-smoke-domain-result.v1"
	AssemblySchemaVersion     = "ocservia.g6-harness-smoke-assembly-result.v1"
	VerificationSchemaVersion = "ocservia.g6-harness-smoke-verification-result.v1"
	SchemaVersion             = "ocservia.g6-harness-smoke-result.v1"
)

var bootIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type ArtifactReference struct {
	ID     int64  `json:"artifact_id"`
	Digest string `json:"artifact_digest"`
}

type Binding struct {
	CandidateSHA  string `json:"candidate_sha"`
	RunID         string `json:"run_id"`
	RunAttempt    int    `json:"run_attempt"`
	EnvironmentID string `json:"environment_id"`
	Authority     string `json:"authority"`
}

type DomainResult struct {
	SchemaVersion  string         `json:"schema_version"`
	Profile        string         `json:"profile"`
	Binding        Binding        `json:"binding"`
	Domain         string         `json:"failure_domain"`
	RunnerName     string         `json:"runner_name"`
	RunnerBootID   string         `json:"runner_boot_id"`
	HarnessSHA256  string         `json:"harness_sha256"`
	EvidenceSHA256 string         `json:"evidence_sha256"`
	EvidenceFiles  int            `json:"evidence_files"`
	Claims         map[string]any `json:"claims"`
	StartedAt      time.Time      `json:"started_at"`
	CompletedAt    time.Time      `json:"completed_at"`
	Status         string         `json:"status"`
}

type Domains struct {
	FDA DomainResult `json:"fd_a"`
	FDB DomainResult `json:"fd_b"`
}

type Artifacts struct {
	Release *ArtifactReference `json:"release"`
	FDA     *ArtifactReference `json:"fd_a"`
	FDB     *ArtifactReference `json:"fd_b"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Result struct {
	SchemaVersion         string    `json:"schema_version"`
	Profile               string    `json:"profile"`
	Binding               Binding   `json:"binding"`
	HarnessSHA256         *string   `json:"harness_sha256"`
	StartedAt             time.Time `json:"started_at"`
	CompletedAt           time.Time `json:"completed_at"`
	Domains               *Domains  `json:"domains"`
	Artifacts             Artifacts `json:"artifacts"`
	Status                string    `json:"status"`
	FormalVerdictEligible bool      `json:"formal_verdict_eligible"`
	Failure               *Failure  `json:"failure"`
}

type DomainOptions struct {
	Binding        rendezvous.Binding
	Domain         string
	RunnerName     string
	BootIDPath     string
	ExecutablePath string
	ExpectedSHA256 string
	EvidenceRoot   string
	Now            func() time.Time
}

func RunDomain(options DomainOptions) (DomainResult, error) {
	if options.Now == nil {
		return DomainResult{}, errors.New("smoke clock is required")
	}
	started := options.Now().UTC()
	result := DomainResult{
		SchemaVersion: DomainSchemaVersion,
		Profile:       "smoke",
		Binding:       resultBinding(options.Binding),
		Domain:        options.Domain,
		RunnerName:    options.RunnerName,
		StartedAt:     started,
		CompletedAt:   started,
		Status:        "failed",
	}
	if err := validateDomainOptions(options); err != nil {
		return result, err
	}
	bootID, err := readBounded(options.BootIDPath, 128)
	if err != nil {
		return result, fmt.Errorf("read runner boot ID: %w", err)
	}
	result.RunnerBootID = strings.TrimSpace(string(bootID))
	if !bootIDPattern.MatchString(result.RunnerBootID) {
		return result, errors.New("runner boot ID is not a lowercase UUID")
	}
	digest, err := digestFile(options.ExecutablePath)
	if err != nil {
		return result, fmt.Errorf("hash frozen harness: %w", err)
	}
	result.HarnessSHA256 = digest
	if digest != options.ExpectedSHA256 {
		return result, errors.New("frozen harness digest does not match the release manifest")
	}
	observation, err := readObservation(filepath.Join(options.EvidenceRoot, "smoke-observations.json"))
	if err != nil {
		return result, fmt.Errorf("validate smoke observations: %w", err)
	}
	if observation.Profile != "smoke" || observation.CandidateSHA != options.Binding.CandidateSHA ||
		observation.EnvironmentID != options.Binding.EnvironmentID || observation.Domain != options.Domain {
		return result, errors.New("smoke observations violate the exact candidate, environment, profile, or domain binding")
	}
	if len(observation.Claims) == 0 {
		return result, errors.New("smoke observations contain no claims")
	}
	evidenceDigest, files, err := digestTree(options.EvidenceRoot)
	if err != nil {
		return result, fmt.Errorf("hash raw smoke evidence: %w", err)
	}
	result.EvidenceSHA256, result.EvidenceFiles, result.Claims = evidenceDigest, files, observation.Claims
	result.CompletedAt = options.Now().UTC()
	result.Status = "passed"
	return result, nil
}

type AggregateOptions struct {
	Binding            rendezvous.Binding
	FDAPath            string
	FDBPath            string
	ReleaseArtifact    ArtifactReference
	FDAArtifact        ArtifactReference
	FDBArtifact        ArtifactReference
	ExpectedHarnessSHA string
	Now                func() time.Time
}

func Aggregate(options AggregateOptions) (Result, error) {
	if options.Now == nil {
		return Result{}, errors.New("smoke clock is required")
	}
	now := options.Now().UTC()
	result := Result{
		SchemaVersion:         SchemaVersion,
		Profile:               "smoke",
		Binding:               resultBinding(options.Binding),
		StartedAt:             now,
		CompletedAt:           now,
		Artifacts:             Artifacts{},
		Status:                "failed",
		FormalVerdictEligible: false,
	}
	fail := func(code string, err error) (Result, error) {
		result.Failure = &Failure{Code: code, Message: err.Error()}
		return result, err
	}
	if err := options.Binding.Validate(); err != nil || options.Binding.Authority != "engineering" {
		if err == nil {
			err = errors.New("smoke authority must be engineering")
		}
		return fail("invalid_binding", err)
	}
	if !isDigest(options.ExpectedHarnessSHA) {
		return fail("invalid_harness_digest", errors.New("expected harness digest is invalid"))
	}
	result.HarnessSHA256 = &options.ExpectedHarnessSHA
	for _, artifact := range []ArtifactReference{options.ReleaseArtifact, options.FDAArtifact, options.FDBArtifact} {
		if artifact.ID < 1 || !isDigest(artifact.Digest) {
			return fail("invalid_artifact_binding", errors.New("smoke artifact binding is incomplete"))
		}
	}
	result.Artifacts = Artifacts{Release: &options.ReleaseArtifact, FDA: &options.FDAArtifact, FDB: &options.FDBArtifact}
	fdA, err := readDomain(options.FDAPath)
	if err != nil {
		return fail("fd_a_result_rejected", err)
	}
	fdB, err := readDomain(options.FDBPath)
	if err != nil {
		return fail("fd_b_result_rejected", err)
	}
	result.Domains = &Domains{FDA: fdA, FDB: fdB}
	result.StartedAt = earlier(fdA.StartedAt, fdB.StartedAt)
	if err := validateDomain(fdA, "fd-a", options.Binding, options.ExpectedHarnessSHA); err != nil {
		return fail("fd_a_result_rejected", err)
	}
	if err := validateDomain(fdB, "fd-b", options.Binding, options.ExpectedHarnessSHA); err != nil {
		return fail("fd_b_result_rejected", err)
	}
	if fdA.RunnerBootID == fdB.RunnerBootID {
		return fail("failure_domains_not_distinct", errors.New("smoke failure domains ran on the same host boot identity"))
	}
	result.Status = "passed"
	result.Failure = nil
	return result, nil
}

func Write(path string, value any) error {
	if !filepath.IsAbs(path) {
		return errors.New("smoke result path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".g6-smoke-result-*")
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
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func validateDomainOptions(options DomainOptions) error {
	if err := options.Binding.Validate(); err != nil {
		return err
	}
	if options.Binding.Authority != "engineering" {
		return errors.New("smoke domain authority must be engineering")
	}
	if options.Domain != "fd-a" && options.Domain != "fd-b" {
		return errors.New("smoke failure domain must be fd-a or fd-b")
	}
	if options.RunnerName == "" || options.Now == nil || !filepath.IsAbs(options.BootIDPath) || !filepath.IsAbs(options.ExecutablePath) || !filepath.IsAbs(options.EvidenceRoot) {
		return errors.New("smoke runner identity, clock, boot ID, and executable are required")
	}
	if !isDigest(options.ExpectedSHA256) {
		return errors.New("expected harness digest is invalid")
	}
	return nil
}

func validateDomain(result DomainResult, domain string, binding rendezvous.Binding, harnessSHA string) error {
	if result.SchemaVersion != DomainSchemaVersion || result.Profile != "smoke" || result.Domain != domain || result.Binding != resultBinding(binding) ||
		result.RunnerName == "" || !bootIDPattern.MatchString(result.RunnerBootID) || result.HarnessSHA256 != harnessSHA ||
		!isDigest(result.EvidenceSHA256) || result.EvidenceFiles < 2 || len(result.Claims) == 0 ||
		result.Status != "passed" || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) {
		return errors.New("smoke domain result violates its exact binding or passed contract")
	}
	return nil
}

type observation struct {
	SchemaVersion string         `json:"schema_version"`
	Profile       string         `json:"profile"`
	CandidateSHA  string         `json:"candidate_sha"`
	EnvironmentID string         `json:"environment_id"`
	Domain        string         `json:"failure_domain"`
	Claims        map[string]any `json:"claims"`
}

func readObservation(path string) (observation, error) {
	var value observation
	if err := readStrictJSON(path, 64<<10, &value); err != nil {
		return value, err
	}
	if value.SchemaVersion != "ocservia.g6-smoke-observations.v1" {
		return value, errors.New("unsupported smoke observation schema")
	}
	return value, nil
}

func digestTree(root string) (string, int, error) {
	if !filepath.IsAbs(root) {
		return "", 0, errors.New("evidence root must be absolute")
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("raw smoke evidence contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("raw smoke evidence contains a non-regular file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	if len(paths) < 2 || len(paths) > 2048 {
		return "", 0, errors.New("raw smoke evidence file count is outside its bounded contract")
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, relative := range paths {
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return "", 0, err
		}
		if info.Size() > 32<<20 {
			return "", 0, errors.New("raw smoke evidence file exceeds 32 MiB")
		}
		digest, err := digestFile(path)
		if err != nil {
			return "", 0, err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%s\n", relative, info.Size(), digest)
	}
	return hex.EncodeToString(hash.Sum(nil)), len(paths), nil
}

func readStrictJSON(path string, limit int64, destination any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("JSON input must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, limit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("JSON input contains trailing data")
	}
	return nil
}

func resultBinding(binding rendezvous.Binding) Binding {
	return Binding{
		CandidateSHA: binding.CandidateSHA, RunID: binding.RunID, RunAttempt: binding.RunAttempt,
		EnvironmentID: binding.EnvironmentID, Authority: binding.Authority,
	}
}

func readDomain(path string) (DomainResult, error) {
	if !filepath.IsAbs(path) {
		return DomainResult{}, errors.New("smoke domain result path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return DomainResult{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return DomainResult{}, errors.New("smoke domain result must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return DomainResult{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var result DomainResult
	if err := decoder.Decode(&result); err != nil {
		return DomainResult{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return DomainResult{}, errors.New("smoke domain result contains trailing JSON")
	}
	return result, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
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

func NormalizeDigest(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "sha256:")
}

func ParseArtifactReference(id, digest string) (ArtifactReference, error) {
	var reference ArtifactReference
	parsed, err := strconv.ParseInt(id, 10, 64)
	if err != nil || parsed < 1 {
		return ArtifactReference{}, errors.New("artifact ID must be a positive integer")
	}
	reference.ID = parsed
	reference.Digest = NormalizeDigest(digest)
	if !isDigest(reference.Digest) {
		return ArtifactReference{}, errors.New("artifact digest must be SHA-256")
	}
	return reference, nil
}

func isDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func earlier(values ...time.Time) time.Time {
	sort.Slice(values, func(i, j int) bool { return values[i].Before(values[j]) })
	return values[0]
}
