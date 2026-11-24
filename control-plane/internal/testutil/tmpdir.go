package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// PrivateTempDir returns a short, mode-0700 temporary directory outside the
// repository tree whose entire ancestor chain is owned by root or the current
// user and is neither group- nor world-writable. This satisfies strict Unix
// socket and secret-file ancestry checks on hosts where /tmp is 1777 or the
// repository checkout itself is group-writable (e.g. 0775).
func PrivateTempDir(t testing.TB) string {
	t.Helper()
	repoRoot := findRepoRoot()
	euid := uint32(os.Geteuid())
	for _, candidate := range privateTempCandidates() {
		if candidate.path == "" {
			continue
		}
		if candidate.create {
			if err := os.MkdirAll(candidate.path, 0o700); err != nil {
				continue
			}
		}
		resolved, err := filepath.EvalSymlinks(candidate.path)
		if err != nil {
			continue
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			continue
		}
		if repoRoot != "" && isWithinDir(resolved, repoRoot) {
			continue
		}
		if !hasSafeAncestry(resolved, euid) {
			continue
		}
		dir, err := os.MkdirTemp(resolved, "ocservia-t-")
		if err != nil {
			continue
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			_ = os.RemoveAll(dir)
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	t.Fatal("no private temporary directory with non-group/world-writable ancestry outside the repository; set TMPDIR to a private 0700 directory")
	return ""
}

type tempCandidate struct {
	path   string
	create bool
}

func privateTempCandidates() []tempCandidate {
	var candidates []tempCandidate
	if tmpdir := strings.TrimSpace(os.Getenv("TMPDIR")); tmpdir != "" {
		candidates = append(candidates, tempCandidate{path: tmpdir})
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		candidates = append(candidates, tempCandidate{path: runtimeDir})
	}
	// Explicitly use $HOME/.cache rather than os.UserCacheDir(), because
	// scripts/env.sh sets XDG_CACHE_HOME inside the repository checkout.
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates, tempCandidate{
			path:   filepath.Join(home, ".cache"),
			create: true,
		})
	}
	return candidates
}

func findRepoRoot() string {
	if envRoot := strings.TrimSpace(os.Getenv("OCSERVIA_ROOT")); envRoot != "" {
		if resolved, err := filepath.EvalSymlinks(envRoot); err == nil {
			if abs, err := filepath.Abs(resolved); err == nil {
				return abs
			}
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	for current := wd; ; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, "go.work")); err == nil {
			return current
		}
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		next := filepath.Dir(current)
		if next == current {
			return ""
		}
	}
}

func isWithinDir(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func hasSafeAncestry(dir string, expectedOwner uint32) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (uint32(stat.Uid) != 0 && uint32(stat.Uid) != expectedOwner) || info.Mode().Perm()&0o022 != 0 {
			return false
		}
		next := filepath.Dir(current)
		if next == current {
			return true
		}
	}
}
