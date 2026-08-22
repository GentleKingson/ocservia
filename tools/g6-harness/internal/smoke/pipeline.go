package smoke

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/rendezvous"
)

const BundleSchemaVersion = "ocservia.g6-harness-smoke-bundle.v1"

type Bundle struct {
	SchemaVersion string    `json:"schema_version"`
	Profile       string    `json:"profile"`
	Binding       Binding   `json:"binding"`
	HarnessSHA256 string    `json:"harness_sha256"`
	Domains       Domains   `json:"domains"`
	Artifacts     Artifacts `json:"artifacts"`
}

type AssemblyResult struct {
	SchemaVersion         string   `json:"schema_version"`
	Profile               string   `json:"profile"`
	Binding               Binding  `json:"binding"`
	BundleSHA256          *string  `json:"bundle_sha256"`
	Status                string   `json:"status"`
	FormalVerdictEligible bool     `json:"formal_verdict_eligible"`
	Failure               *Failure `json:"failure"`
}

type VerificationResult struct {
	SchemaVersion         string   `json:"schema_version"`
	Profile               string   `json:"profile"`
	Binding               Binding  `json:"binding"`
	BundleSHA256          *string  `json:"bundle_sha256"`
	Status                string   `json:"status"`
	FormalVerdictEligible bool     `json:"formal_verdict_eligible"`
	Failure               *Failure `json:"failure"`
}

type AssembleOptions struct {
	Binding                                          rendezvous.Binding
	FDAPath, FDBPath, BundlePath, ExpectedHarnessSHA string
	ReleaseArtifact, FDAArtifact, FDBArtifact        ArtifactReference
}

func Assemble(options AssembleOptions) (AssemblyResult, error) {
	result := AssemblyResult{SchemaVersion: AssemblySchemaVersion, Profile: "smoke", Binding: resultBinding(options.Binding), Status: "failed"}
	fail := func(code string, err error) (AssemblyResult, error) {
		result.Failure = &Failure{Code: code, Message: err.Error()}
		return result, err
	}
	if err := options.Binding.Validate(); err != nil || options.Binding.Authority != "engineering" {
		if err == nil {
			err = errors.New("smoke assembly authority must be engineering")
		}
		return fail("invalid_binding", err)
	}
	if !filepath.IsAbs(options.BundlePath) || !isDigest(options.ExpectedHarnessSHA) {
		return fail("invalid_options", errors.New("absolute bundle path and harness digest are required"))
	}
	for _, artifact := range []ArtifactReference{options.ReleaseArtifact, options.FDAArtifact, options.FDBArtifact} {
		if artifact.ID < 1 || !isDigest(artifact.Digest) {
			return fail("invalid_artifact_binding", errors.New("assembly artifact binding is incomplete"))
		}
	}
	fdA, err := readDomain(options.FDAPath)
	if err != nil {
		return fail("fd_a_result_rejected", err)
	}
	fdB, err := readDomain(options.FDBPath)
	if err != nil {
		return fail("fd_b_result_rejected", err)
	}
	if err := validateDomain(fdA, "fd-a", options.Binding, options.ExpectedHarnessSHA); err != nil {
		return fail("fd_a_result_rejected", err)
	}
	if err := validateDomain(fdB, "fd-b", options.Binding, options.ExpectedHarnessSHA); err != nil {
		return fail("fd_b_result_rejected", err)
	}
	for _, domain := range []struct {
		path   string
		result DomainResult
	}{{options.FDAPath, fdA}, {options.FDBPath, fdB}} {
		digest, files, err := digestTree(filepath.Dir(domain.path))
		if err != nil || digest != domain.result.EvidenceSHA256 || files != domain.result.EvidenceFiles {
			if err == nil {
				err = errors.New("raw evidence tree does not match its domain result digest and file count")
			}
			return fail("raw_evidence_rejected", err)
		}
	}
	if fdA.RunnerBootID == fdB.RunnerBootID {
		return fail("failure_domains_not_distinct", errors.New("smoke failure domains share one host boot identity"))
	}
	bundle := Bundle{SchemaVersion: BundleSchemaVersion, Profile: "smoke", Binding: resultBinding(options.Binding), HarnessSHA256: options.ExpectedHarnessSHA, Domains: Domains{FDA: fdA, FDB: fdB}, Artifacts: Artifacts{Release: &options.ReleaseArtifact, FDA: &options.FDAArtifact, FDB: &options.FDBArtifact}}
	if err := Write(options.BundlePath, bundle); err != nil {
		return fail("bundle_write_failed", err)
	}
	digest, err := digestFile(options.BundlePath)
	if err != nil {
		return fail("bundle_hash_failed", err)
	}
	result.BundleSHA256, result.Status = &digest, "passed"
	return result, nil
}

type VerifyOptions struct {
	Binding                                           rendezvous.Binding
	BundlePath, ExpectedBundleSHA, ExpectedHarnessSHA string
}

func Verify(options VerifyOptions) (VerificationResult, error) {
	result := VerificationResult{SchemaVersion: VerificationSchemaVersion, Profile: "smoke", Binding: resultBinding(options.Binding), Status: "failed"}
	fail := func(code string, err error) (VerificationResult, error) {
		result.Failure = &Failure{Code: code, Message: err.Error()}
		return result, err
	}
	if err := options.Binding.Validate(); err != nil || options.Binding.Authority != "engineering" {
		if err == nil {
			err = errors.New("smoke verifier authority must be engineering")
		}
		return fail("invalid_binding", err)
	}
	actualDigest, err := digestFile(options.BundlePath)
	if err != nil {
		return fail("bundle_read_failed", err)
	}
	result.BundleSHA256 = &actualDigest
	if actualDigest != options.ExpectedBundleSHA || !isDigest(options.ExpectedBundleSHA) {
		return fail("bundle_digest_mismatch", fmt.Errorf("bundle digest %s does not match expected %s", actualDigest, options.ExpectedBundleSHA))
	}
	var bundle Bundle
	if err := readStrictJSON(options.BundlePath, 1<<20, &bundle); err != nil {
		return fail("bundle_rejected", err)
	}
	if bundle.SchemaVersion != BundleSchemaVersion || bundle.Profile != "smoke" || bundle.Binding != resultBinding(options.Binding) || bundle.HarnessSHA256 != options.ExpectedHarnessSHA {
		return fail("bundle_rejected", errors.New("bundle violates its exact smoke binding"))
	}
	if err := validateDomain(bundle.Domains.FDA, "fd-a", options.Binding, options.ExpectedHarnessSHA); err != nil {
		return fail("fd_a_result_rejected", err)
	}
	if err := validateDomain(bundle.Domains.FDB, "fd-b", options.Binding, options.ExpectedHarnessSHA); err != nil {
		return fail("fd_b_result_rejected", err)
	}
	if bundle.Domains.FDA.RunnerBootID == bundle.Domains.FDB.RunnerBootID {
		return fail("failure_domains_not_distinct", errors.New("bundle domains share one host boot identity"))
	}
	for _, artifact := range []*ArtifactReference{bundle.Artifacts.Release, bundle.Artifacts.FDA, bundle.Artifacts.FDB} {
		if artifact == nil || artifact.ID < 1 || !isDigest(artifact.Digest) {
			return fail("artifact_binding_rejected", errors.New("bundle artifact binding is incomplete"))
		}
	}
	result.Status = "passed"
	return result, nil
}

func BundleDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("bundle must be a regular file")
	}
	return digestFile(path)
}
