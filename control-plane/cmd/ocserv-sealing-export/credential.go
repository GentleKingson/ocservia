package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func readCredentialFile(path string) ([]byte, error) {
	file, err := openCredentialFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) == 0 || len(raw) > 4096 {
		clear(raw)
		return nil, errors.New("credential must contain 1..4096 bytes")
	}
	return raw, nil
}

// Walk verified directory descriptors rather than checking and reopening paths.
// This follows the Controller's strict command-signing key ancestry contract.
func openCredentialFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("credential path must be absolute and canonical")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	uid := uint32(os.Geteuid())
	directory, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(directory) }()
	for i := 0; ; i++ {
		var stat unix.Stat_t
		if err := unix.Fstat(directory, &stat); err != nil {
			return nil, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR || (stat.Uid != 0 && stat.Uid != uid) || stat.Mode&0022 != 0 {
			return nil, errors.New("credential ancestry must be root- or process-owned and not group/world writable")
		}
		if i == len(parts)-1 {
			break
		}
		next, err := unix.Openat(directory, parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		_ = unix.Close(directory)
		directory = next
	}
	fd, err := unix.Openat(directory, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	mode := stat.Mode & 07777
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || (stat.Uid != 0 && stat.Uid != uid) || stat.Nlink != 1 ||
		(mode != 0400 && mode != 0600) || stat.Size < 1 || stat.Size > 4096 {
		file.Close()
		return nil, errors.New("credential must be private, root- or process-owned, regular, bounded and single-linked")
	}
	return file, nil
}
