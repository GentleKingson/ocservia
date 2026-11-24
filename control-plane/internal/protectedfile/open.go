// Package protectedfile opens files beneath directories controlled by root or the caller.
package protectedfile

import (
	"errors"
	"fmt"
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
