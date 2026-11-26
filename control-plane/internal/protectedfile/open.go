// Package protectedfile opens files beneath directories controlled by root or the caller.
package protectedfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Open walks the canonical path using directory descriptors. The caller owns the
// returned file and checks the opened file against its own credential policy.
func Open(path string, uid uint32) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("protected file path must be absolute and canonical")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	directory, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open protected file root: %w", err)
	}
	defer func() { _ = unix.Close(directory) }()
	for i := 0; ; i++ {
		var stat unix.Stat_t
		if err := unix.Fstat(directory, &stat); err != nil {
			return nil, fmt.Errorf("stat protected file ancestor: %w", err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || (stat.Uid != 0 && stat.Uid != uid) || stat.Mode&0o022 != 0 {
			return nil, errors.New("protected file ancestry must be root- or process-owned and not group/world writable")
		}
		if i == len(parts)-1 {
			break
		}
		next, err := unix.Openat(directory, parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open protected file ancestor: %w", err)
		}
		_ = unix.Close(directory)
		directory = next
	}
	fd, err := unix.Openat(directory, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open protected file: %w", err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// OpenPrivate opens path under a safe directory hierarchy and verifies that it
// is a single-linked regular private file (mode 0400 or 0600) owned by
// expectedUID (or root when allowRoot is true) with size in 1..maxBytes.
func OpenPrivate(path string, expectedUID uint32, allowRoot bool, maxBytes int64) (*os.File, error) {
	file, err := Open(path, expectedUID)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		file.Close()
		return nil, fmt.Errorf("stat protected file: %w", err)
	}
	mode := stat.Mode & 0o7777
	uidOK := stat.Uid == expectedUID || (allowRoot && stat.Uid == 0)
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || !uidOK || stat.Nlink != 1 || (mode != 0o400 && mode != 0o600) {
		file.Close()
		return nil, errors.New("protected file must be a private, regular, single-linked file owned by an authorized UID")
	}
	if stat.Size < 1 || stat.Size > maxBytes {
		file.Close()
		return nil, fmt.Errorf("protected file must contain 1..%d bytes", maxBytes)
	}
	return file, nil
}

// ReadPrivate opens path via OpenPrivate and reads its bounded contents.
func ReadPrivate(path string, expectedUID uint32, allowRoot bool, maxBytes int64) ([]byte, error) {
	file, err := OpenPrivate(path, expectedUID, allowRoot, maxBytes)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		clear(raw)
		return nil, fmt.Errorf("read protected file: %w", err)
	}
	if len(raw) == 0 || int64(len(raw)) > maxBytes {
		clear(raw)
		return nil, fmt.Errorf("protected file must contain 1..%d bytes", maxBytes)
	}
	return raw, nil
}
