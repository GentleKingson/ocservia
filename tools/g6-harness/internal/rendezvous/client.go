package rendezvous

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxArchiveBytes       = int64(256 << 20)
	maxExpandedBytes      = int64(1 << 30)
	maxCheckpointFiles    = 4096
	defaultRequestTimeout = 20 * time.Second
)

type Failure struct {
	Class    string `json:"class"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

func (f *Failure) Error() string { return f.Message }

type ArtifactReference struct {
	ID     int64  `json:"artifact_id"`
	Digest string `json:"artifact_digest"`
}

type Result struct {
	SchemaVersion  string             `json:"schema_version"`
	ArtifactName   string             `json:"artifact_name"`
	Checkpoint     string             `json:"checkpoint"`
	ProducerDomain string             `json:"producer_domain"`
	CandidateSHA   string             `json:"candidate_sha"`
	RunID          string             `json:"run_id"`
	RunAttempt     int                `json:"run_attempt"`
	EnvironmentID  string             `json:"environment_id"`
	Authority      string             `json:"authority"`
	StartedAt      time.Time          `json:"started_at"`
	CompletedAt    time.Time          `json:"completed_at"`
	Status         string             `json:"status"`
	Artifact       *ArtifactReference `json:"artifact,omitempty"`
	ManifestSHA256 string             `json:"manifest_sha256,omitempty"`
	Failure        *Failure           `json:"failure,omitempty"`
}

type Options struct {
	BaseURL              string
	Repository           string
	Token                string
	Binding              Binding
	ArtifactName         string
	Destination          string
	PeerJob              string
	StatePath            string
	Timeout              time.Duration
	PollInterval         time.Duration
	PropagationGrace     time.Duration
	RequestTimeout       time.Duration
	DownloadRetryTotal   time.Duration
	MaxConsecutiveErrors int
	HTTPClient           *http.Client
	Now                  func() time.Time
	Sleep                func(context.Context, time.Duration) error
}

type artifactList struct {
	TotalCount int        `json:"total_count"`
	Artifacts  []artifact `json:"artifacts"`
}

type artifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Expired bool   `json:"expired"`
	Digest  string `json:"digest"`
}

type jobsList struct {
	Jobs []job `json:"jobs"`
}

type job struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Steps      []step `json:"steps"`
}

type step struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type consumerState struct {
	SchemaVersion  string   `json:"schema_version"`
	ProducerDomain string   `json:"producer_domain"`
	CandidateSHA   string   `json:"candidate_sha"`
	RunID          string   `json:"run_id"`
	RunAttempt     int      `json:"run_attempt"`
	EnvironmentID  string   `json:"environment_id"`
	Authority      string   `json:"authority"`
	LastSequence   int      `json:"last_sequence"`
	Checkpoints    []string `json:"checkpoints"`
}

func WaitDownload(ctx context.Context, options Options) (result Result, err error) {
	options.setDefaults()
	started := options.Now().UTC()
	contract, contractErr := ResolveContract(options.ArtifactName, options.Binding)
	result = Result{
		SchemaVersion: ResultSchemaVersion,
		ArtifactName:  options.ArtifactName,
		CandidateSHA:  options.Binding.CandidateSHA,
		RunID:         options.Binding.RunID,
		RunAttempt:    options.Binding.RunAttempt,
		EnvironmentID: options.Binding.EnvironmentID,
		Authority:     options.Binding.Authority,
		StartedAt:     started,
		Status:        "failed",
	}
	if contractErr != nil {
		failure := failure("harness_contract_failed", "invalid_artifact_contract", contractErr.Error())
		result.Failure = failure
		result.CompletedAt = options.Now().UTC()
		return result, failure
	}
	result.Checkpoint = contract.Checkpoint
	result.ProducerDomain = contract.ProducerDomain
	defer func() { result.CompletedAt = options.Now().UTC() }()
	if err := options.validate(contract); err != nil {
		failure := failure("harness_contract_failed", "invalid_wait_configuration", err.Error())
		result.Failure = failure
		return result, failure
	}

	waitContext, cancel := context.WithTimeoutCause(ctx, options.Timeout, errors.New("checkpoint wait deadline exceeded"))
	defer cancel()
	selected, waitErr := waitForArtifact(waitContext, options)
	if waitErr != nil {
		result.Failure = asFailure(waitErr)
		return result, waitErr
	}
	archive, digest, downloadErr := downloadArtifact(ctx, options, selected)
	if downloadErr != nil {
		result.Failure = asFailure(downloadErr)
		return result, downloadErr
	}
	defer os.Remove(archive)
	staging, extractErr := extractArchive(archive, options.Destination)
	if extractErr != nil {
		result.Failure = asFailure(extractErr)
		return result, extractErr
	}
	defer os.RemoveAll(staging)
	manifest, manifestErr := ReadAndValidateManifest(staging, options.ArtifactName, options.Binding, options.Now())
	if manifestErr != nil {
		wrapped := failure("harness_contract_failed", "checkpoint_manifest_rejected", manifestErr.Error())
		result.Failure = wrapped
		return result, wrapped
	}
	if stateErr := advanceState(options.StatePath, manifest); stateErr != nil {
		wrapped := failure("harness_contract_failed", "checkpoint_sequence_rejected", stateErr.Error())
		result.Failure = wrapped
		return result, wrapped
	}
	if installErr := installDestination(staging, options.Destination); installErr != nil {
		wrapped := failure("runner_infrastructure_failed", "checkpoint_install_failed", installErr.Error())
		result.Failure = wrapped
		return result, wrapped
	}
	manifestDigest, digestErr := digestFile(filepath.Join(options.Destination, ManifestFilename))
	if digestErr != nil {
		wrapped := failure("runner_infrastructure_failed", "manifest_digest_failed", digestErr.Error())
		result.Failure = wrapped
		return result, wrapped
	}
	result.Status = "passed"
	result.Artifact = &ArtifactReference{ID: selected.ID, Digest: digest}
	result.ManifestSHA256 = manifestDigest
	return result, nil
}

func (options *Options) setDefaults() {
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.PollInterval == 0 {
		options.PollInterval = 5 * time.Second
	}
	if options.PropagationGrace == 0 {
		options.PropagationGrace = 30 * time.Second
	}
	if options.DownloadRetryTotal == 0 {
		options.DownloadRetryTotal = 90 * time.Second
	}
	if options.MaxConsecutiveErrors == 0 {
		options.MaxConsecutiveErrors = 3
	}
}

func (options Options) validate(contract Contract) error {
	if options.BaseURL == "" || options.Repository == "" || options.Token == "" {
		return errors.New("GitHub API URL, repository, and token are required")
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return errors.New("GitHub API URL is invalid")
	}
	if !filepath.IsAbs(options.Destination) || !filepath.IsAbs(options.StatePath) {
		return errors.New("destination and state path must be absolute")
	}
	if err := requireEmptyDestination(options.Destination); err != nil {
		return err
	}
	if options.Timeout <= 0 || options.Timeout > time.Hour || options.RequestTimeout <= 0 || options.RequestTimeout > time.Minute ||
		options.PollInterval <= 0 || options.PollInterval > 30*time.Second || options.PropagationGrace <= 0 || options.PropagationGrace > 5*time.Minute ||
		options.DownloadRetryTotal <= 0 || options.DownloadRetryTotal > 5*time.Minute || options.MaxConsecutiveErrors < 1 || options.MaxConsecutiveErrors > 10 {
		return errors.New("rendezvous deadlines or retry limits are outside their bounded contract")
	}
	expectedPeer, err := peerJobName(contract.ProducerDomain)
	if err != nil {
		return err
	}
	if options.PeerJob != expectedPeer {
		return fmt.Errorf("peer job must be %q for producer %s", expectedPeer, contract.ProducerDomain)
	}
	return nil
}

func waitForArtifact(ctx context.Context, options Options) (artifact, error) {
	artifactErrors := 0
	jobsErrors := 0
	var peerSucceededAt time.Time
	for {
		artifacts, artifactErr := listArtifacts(ctx, options)
		if artifactErr != nil {
			var contractFailure *Failure
			if errors.As(artifactErr, &contractFailure) && contractFailure.Class == "harness_contract_failed" {
				return artifact{}, contractFailure
			}
			artifactErrors++
			if artifactErrors >= options.MaxConsecutiveErrors {
				return artifact{}, failure("runner_infrastructure_failed", "artifact_api_unavailable", fmt.Sprintf("artifact API failed %d consecutive bounded requests: %v", artifactErrors, artifactErr))
			}
		} else {
			artifactErrors = 0
		}
		peerState, jobsErr := inspectPeer(ctx, options)
		if jobsErr != nil {
			var structured *Failure
			if errors.As(jobsErr, &structured) {
				return artifact{}, structured
			}
			jobsErrors++
			if jobsErrors >= options.MaxConsecutiveErrors {
				return artifact{}, failure("runner_infrastructure_failed", "jobs_api_unavailable", fmt.Sprintf("jobs API failed %d consecutive bounded requests: %v", jobsErrors, jobsErr))
			}
		} else {
			jobsErrors = 0
		}
		if artifactErr == nil && jobsErr == nil && len(artifacts) == 1 {
			return artifacts[0], nil
		}
		if jobsErr == nil && peerState == "success" && len(artifacts) == 0 {
			if peerSucceededAt.IsZero() {
				peerSucceededAt = options.Now()
			} else if options.Now().Sub(peerSucceededAt) >= options.PropagationGrace {
				return artifact{}, failure("peer_failed", "peer_checkpoint_missing", fmt.Sprintf("peer job %s succeeded but did not publish %s within the propagation grace period", options.PeerJob, options.ArtifactName))
			}
		}
		if sleepErr := options.Sleep(ctx, options.PollInterval); sleepErr != nil {
			return artifact{}, failure("peer_checkpoint_timeout", "checkpoint_wait_timeout", fmt.Sprintf("timed out waiting for checkpoint %s", options.ArtifactName))
		}
	}
}

func listArtifacts(ctx context.Context, options Options) ([]artifact, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/actions/runs/%s/artifacts?per_page=100&name=%s", strings.TrimRight(options.BaseURL, "/"), options.Repository, options.Binding.RunID, url.QueryEscape(options.ArtifactName))
	var response artifactList
	if err := getJSON(ctx, options, endpoint, &response); err != nil {
		return nil, err
	}
	var matches []artifact
	for _, candidate := range response.Artifacts {
		if candidate.Name == options.ArtifactName && !candidate.Expired {
			matches = append(matches, candidate)
		}
	}
	if response.TotalCount > 1 || len(matches) > 1 {
		return nil, failure("harness_contract_failed", "duplicate_artifact", fmt.Sprintf("expected one artifact named %s, found %d", options.ArtifactName, max(response.TotalCount, len(matches))))
	}
	if len(matches) == 1 && (matches[0].ID < 1 || normalizeDigest(matches[0].Digest) == "") {
		return nil, failure("harness_contract_failed", "artifact_metadata_invalid", "artifact metadata is missing a valid ID or SHA-256 digest")
	}
	return matches, nil
}

func inspectPeer(ctx context.Context, options Options) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/actions/runs/%s/attempts/%d/jobs?per_page=100", strings.TrimRight(options.BaseURL, "/"), options.Repository, options.Binding.RunID, options.Binding.RunAttempt)
	var response jobsList
	if err := getJSON(ctx, options, endpoint, &response); err != nil {
		return "", err
	}
	var matches []job
	for _, candidate := range response.Jobs {
		if candidate.Name == options.PeerJob {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return "", failure("harness_contract_failed", "peer_job_ambiguous", fmt.Sprintf("expected exactly one peer job named %s, found %d", options.PeerJob, len(matches)))
	}
	peer := matches[0]
	for _, candidate := range peer.Steps {
		if candidate.Status == "completed" && terminalFailure(candidate.Conclusion) {
			return "", failure("peer_failed", "peer_step_failed", fmt.Sprintf("peer job %s failed at step %s (%s)", options.PeerJob, candidate.Name, candidate.Conclusion))
		}
	}
	if peer.Status == "completed" {
		if peer.Conclusion != "success" {
			return "", failure("peer_failed", "peer_job_failed", fmt.Sprintf("peer job %s completed with conclusion %s", options.PeerJob, peer.Conclusion))
		}
		return "success", nil
	}
	return "running", nil
}

func terminalFailure(conclusion string) bool {
	switch conclusion {
	case "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
		return true
	default:
		return false
	}
}

func getJSON(ctx context.Context, options Options, endpoint string, destination any) error {
	requestContext, cancel := context.WithTimeout(ctx, options.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+options.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := options.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("GitHub API returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode GitHub API response: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("GitHub API response contains trailing JSON")
	}
	return nil
}

func downloadArtifact(ctx context.Context, options Options, selected artifact) (string, string, error) {
	digest := normalizeDigest(selected.Digest)
	if digest == "" {
		return "", "", failure("harness_contract_failed", "artifact_digest_invalid", "artifact metadata digest is not a SHA-256 value")
	}
	downloadContext, cancel := context.WithTimeoutCause(ctx, options.DownloadRetryTotal, errors.New("artifact download deadline exceeded"))
	defer cancel()
	deadline := options.Now().Add(options.DownloadRetryTotal)
	failures := 0
	for {
		archive, actual, err := downloadOnce(downloadContext, options, selected.ID)
		if err == nil {
			if actual != digest {
				os.Remove(archive)
				return "", "", failure("harness_contract_failed", "artifact_digest_mismatch", fmt.Sprintf("artifact %d digest mismatch", selected.ID))
			}
			return archive, digest, nil
		}
		failures++
		remaining := deadline.Sub(options.Now())
		if remaining <= 0 {
			return "", "", failure("runner_infrastructure_failed", "artifact_download_failed", fmt.Sprintf("artifact download failed after %d bounded requests", failures))
		}
		delay := min(options.PollInterval, remaining)
		if sleepErr := options.Sleep(downloadContext, delay); sleepErr != nil {
			return "", "", failure("runner_infrastructure_failed", "artifact_download_timeout", "artifact download deadline exceeded")
		}
	}
}

func downloadOnce(ctx context.Context, options Options, artifactID int64) (string, string, error) {
	requestContext, cancel := context.WithTimeout(ctx, options.RequestTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/repos/%s/actions/artifacts/%d/zip", strings.TrimRight(options.BaseURL, "/"), options.Repository, artifactID)
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Set("Authorization", "Bearer "+options.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := options.HTTPClient.Do(request)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", "", fmt.Errorf("artifact download returned HTTP %d", response.StatusCode)
	}
	archiveDirectory := filepath.Dir(options.Destination)
	if err := os.MkdirAll(archiveDirectory, 0o700); err != nil {
		return "", "", err
	}
	temporary, err := os.CreateTemp(archiveDirectory, ".g6-checkpoint-*.zip")
	if err != nil {
		return "", "", err
	}
	name := temporary.Name()
	defer temporary.Close()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, maxArchiveBytes+1))
	if copyErr != nil {
		os.Remove(name)
		return "", "", copyErr
	}
	if written > maxArchiveBytes {
		os.Remove(name)
		return "", "", errors.New("artifact archive exceeds the bounded size limit")
	}
	if err := temporary.Sync(); err != nil {
		os.Remove(name)
		return "", "", err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return "", "", err
	}
	return name, hex.EncodeToString(hash.Sum(nil)), nil
}

func extractArchive(archive, destination string) (string, error) {
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", failure("runner_infrastructure_failed", "checkpoint_staging_failed", err.Error())
	}
	staging, err := os.MkdirTemp(parent, ".g6-checkpoint-*")
	if err != nil {
		return "", failure("runner_infrastructure_failed", "checkpoint_staging_failed", err.Error())
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		os.RemoveAll(staging)
		return "", failure("harness_contract_failed", "artifact_zip_invalid", "artifact download is not a valid ZIP archive")
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > maxCheckpointFiles {
		os.RemoveAll(staging)
		return "", failure("harness_contract_failed", "artifact_file_count_invalid", "artifact ZIP file count is outside the bounded contract")
	}
	var declaredExpanded int64
	var expanded int64
	seen := make(map[string]struct{}, len(reader.File))
	for _, entry := range reader.File {
		clean, pathErr := safeArchivePath(entry.Name)
		if pathErr != nil {
			os.RemoveAll(staging)
			return "", failure("harness_contract_failed", "artifact_path_unsafe", pathErr.Error())
		}
		if _, duplicate := seen[clean]; duplicate {
			os.RemoveAll(staging)
			return "", failure("harness_contract_failed", "artifact_member_duplicate", "artifact contains duplicate member paths")
		}
		seen[clean] = struct{}{}
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 || (!entry.FileInfo().Mode().IsRegular() && !entry.FileInfo().IsDir()) {
			os.RemoveAll(staging)
			return "", failure("harness_contract_failed", "artifact_member_unsafe", "artifact contains a non-regular member")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(staging, clean), 0o700); err != nil {
				os.RemoveAll(staging)
				return "", err
			}
			continue
		}
		declaredExpanded += int64(entry.UncompressedSize64)
		if declaredExpanded > maxExpandedBytes {
			os.RemoveAll(staging)
			return "", failure("harness_contract_failed", "artifact_expanded_size_exceeded", "artifact expanded size exceeds the bounded contract")
		}
		target := filepath.Join(staging, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			os.RemoveAll(staging)
			return "", err
		}
		source, err := entry.Open()
		if err != nil {
			os.RemoveAll(staging)
			return "", err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			source.Close()
			os.RemoveAll(staging)
			return "", err
		}
		remaining := maxExpandedBytes - expanded
		written, copyErr := io.Copy(output, io.LimitReader(source, remaining+1))
		closeErr := output.Close()
		source.Close()
		if written > remaining {
			os.RemoveAll(staging)
			return "", failure("harness_contract_failed", "artifact_expanded_size_exceeded", "artifact expanded size exceeds the bounded contract")
		}
		expanded += written
		if copyErr != nil || closeErr != nil {
			os.RemoveAll(staging)
			return "", errors.Join(copyErr, closeErr)
		}
	}
	return staging, nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return "", errors.New("artifact contains an unsafe member name")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", errors.New("artifact contains path traversal")
	}
	return clean, nil
}

func requireEmptyDestination(destination string) error {
	entries, err := os.ReadDir(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("artifact destination must be empty")
	}
	return nil
}

func installDestination(staging, destination string) error {
	if err := requireEmptyDestination(destination); err != nil {
		return err
	}
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	return nil
}

func advanceState(path string, manifest Manifest) error {
	state := consumerState{
		SchemaVersion:  StateSchemaVersion,
		ProducerDomain: manifest.ProducerDomain,
		CandidateSHA:   manifest.CandidateSHA,
		RunID:          manifest.RunID,
		RunAttempt:     manifest.RunAttempt,
		EnvironmentID:  manifest.EnvironmentID,
		Authority:      manifest.Authority,
	}
	file, err := os.Open(path)
	if err == nil {
		decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&state)
		if decodeErr == nil && decoder.Decode(&struct{}{}) != io.EOF {
			decodeErr = errors.New("rendezvous state contains trailing JSON")
		}
		file.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode rendezvous state: %w", decodeErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.SchemaVersion != StateSchemaVersion || state.ProducerDomain != manifest.ProducerDomain ||
		state.CandidateSHA != manifest.CandidateSHA || state.RunID != manifest.RunID ||
		state.RunAttempt != manifest.RunAttempt || state.EnvironmentID != manifest.EnvironmentID ||
		state.Authority != manifest.Authority {
		return errors.New("rendezvous state belongs to another schema, producer, candidate, run, attempt, environment, or authority")
	}
	for _, checkpoint := range state.Checkpoints {
		if checkpoint == manifest.Checkpoint {
			return fmt.Errorf("duplicate checkpoint %s", manifest.Checkpoint)
		}
	}
	if manifest.Sequence <= state.LastSequence {
		return fmt.Errorf("checkpoint sequence %d does not advance %d", manifest.Sequence, state.LastSequence)
	}
	state.LastSequence = manifest.Sequence
	state.Checkpoints = append(state.Checkpoints, manifest.Checkpoint)
	sort.Strings(state.Checkpoints)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeJSONAtomic(path, state)
}

func normalizeDigest(value string) string {
	value = strings.TrimPrefix(strings.ToLower(value), "sha256:")
	if !digestPattern.MatchString(value) {
		return ""
	}
	return value
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func failure(class, code, message string) *Failure {
	return &Failure{Class: class, Code: code, Message: message}
}

func asFailure(err error) *Failure {
	var typed *Failure
	if errors.As(err, &typed) {
		return typed
	}
	return failure("runner_infrastructure_failed", "unexpected_rendezvous_error", err.Error())
}
