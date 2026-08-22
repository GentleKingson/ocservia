package smoke

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GentleKingson/ocservia/tools/g6-harness/internal/rendezvous"
)

func TestSmokeDomainAndAggregateBindDistinctFrozenRunners(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	executable := filepath.Join(root, "ocservia-g6-harness")
	if err := os.WriteFile(executable, []byte("frozen-harness"), 0o700); err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256([]byte("frozen-harness"))
	digest := hex.EncodeToString(digestBytes[:])
	binding := rendezvous.Binding{
		CandidateSHA: "0123456789abcdef0123456789abcdef01234567", RunID: "42", RunAttempt: 3,
		EnvironmentID: rendezvous.EnvironmentID("42", 3), Authority: "engineering",
	}
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	makeDomain := func(domain, bootID string) DomainResult {
		t.Helper()
		bootPath := filepath.Join(root, domain+"-boot-id")
		if err := os.WriteFile(bootPath, []byte(bootID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		clock := base
		result, err := RunDomain(DomainOptions{
			Binding: binding, Domain: domain, RunnerName: "GitHub Actions 1", BootIDPath: bootPath,
			ExecutablePath: executable, ExpectedSHA256: digest,
			Now: func() time.Time { clock = clock.Add(time.Second); return clock },
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	fdA := makeDomain("fd-a", "11111111-1111-1111-1111-111111111111")
	fdB := makeDomain("fd-b", "22222222-2222-2222-2222-222222222222")
	fdAPath := filepath.Join(root, "fd-a.json")
	fdBPath := filepath.Join(root, "fd-b.json")
	if err := Write(fdAPath, fdA); err != nil {
		t.Fatal(err)
	}
	if err := Write(fdBPath, fdB); err != nil {
		t.Fatal(err)
	}
	result, err := Aggregate(AggregateOptions{
		Binding: binding, FDAPath: fdAPath, FDBPath: fdBPath,
		ReleaseArtifact: artifact(1), FDAArtifact: artifact(2), FDBArtifact: artifact(3),
		ExpectedHarnessSHA: digest, Now: func() time.Time { return base.Add(3 * time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "passed" || result.FormalVerdictEligible || result.Failure != nil {
		t.Fatalf("unexpected smoke aggregate: %+v", result)
	}
	if result.Domains == nil || result.Domains.FDA.RunnerBootID == result.Domains.FDB.RunnerBootID {
		t.Fatal("distinct smoke runners collapsed to one boot identity")
	}
}

func TestSmokeAggregateRejectsOneHostAndCrossCandidateResults(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binding := rendezvous.Binding{
		CandidateSHA: "0123456789abcdef0123456789abcdef01234567", RunID: "42", RunAttempt: 3,
		EnvironmentID: rendezvous.EnvironmentID("42", 3), Authority: "engineering",
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	domain := DomainResult{
		SchemaVersion: DomainSchemaVersion, Profile: "smoke", Binding: resultBinding(binding), Domain: "fd-a",
		RunnerName: "runner", RunnerBootID: "11111111-1111-1111-1111-111111111111",
		HarnessSHA256: testDigest, StartedAt: now, CompletedAt: now, Status: "passed",
	}
	fdAPath := filepath.Join(root, "fd-a.json")
	fdBPath := filepath.Join(root, "fd-b.json")
	if err := Write(fdAPath, domain); err != nil {
		t.Fatal(err)
	}
	domain.Domain = "fd-b"
	if err := Write(fdBPath, domain); err != nil {
		t.Fatal(err)
	}
	options := AggregateOptions{
		Binding: binding, FDAPath: fdAPath, FDBPath: fdBPath,
		ReleaseArtifact: artifact(1), FDAArtifact: artifact(2), FDBArtifact: artifact(3),
		ExpectedHarnessSHA: testDigest, Now: func() time.Time { return now },
	}
	result, err := Aggregate(options)
	if err == nil || result.Failure == nil || result.Failure.Code != "failure_domains_not_distinct" {
		t.Fatalf("single-host smoke was accepted: result=%+v err=%v", result, err)
	}
	domain.RunnerBootID = "22222222-2222-2222-2222-222222222222"
	domain.Binding.CandidateSHA = "1123456789abcdef0123456789abcdef01234567"
	if err := Write(fdBPath, domain); err != nil {
		t.Fatal(err)
	}
	result, err = Aggregate(options)
	if err == nil || result.Failure == nil || result.Failure.Code != "fd_b_result_rejected" {
		t.Fatalf("cross-candidate smoke was accepted: result=%+v err=%v", result, err)
	}
}

func TestArtifactReferenceParsingIsExact(t *testing.T) {
	t.Parallel()
	reference, err := ParseArtifactReference("123", "sha256:"+testDigest)
	if err != nil || reference.ID != 123 || reference.Digest != testDigest {
		t.Fatalf("valid artifact reference rejected: %+v %v", reference, err)
	}
	for _, id := range []string{"", "0", "12 trailing", "-1"} {
		if _, err := ParseArtifactReference(id, testDigest); err == nil {
			t.Fatalf("invalid artifact ID %q was accepted", id)
		}
	}
}

func TestSmokeAggregateRejectsUnsafeDomainResultFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, make([]byte, (64<<10)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDomain(target); err == nil {
		t.Fatal("oversized smoke domain result was accepted")
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readDomain(link); err == nil {
		t.Fatal("symlink smoke domain result was accepted")
	}
}

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func artifact(id int64) ArtifactReference {
	return ArtifactReference{ID: id, Digest: testDigest}
}
