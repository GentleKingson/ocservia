package udssecurity

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

type Identity struct {
	Device uint64
	Inode  uint64
}

func ValidateParent(path string, expectedOwner uint32) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("unix socket path must be absolute")
	}
	parent := filepath.Clean(filepath.Dir(path))
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("unix socket ancestry must contain only directories")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (uint32(stat.Uid) != 0 && uint32(stat.Uid) != expectedOwner) || info.Mode().Perm()&0o022 != 0 {
			return "", errors.New("unix socket ancestry must be trusted-owner controlled and not group or world writable")
		}
		if next := filepath.Dir(current); next == current {
			break
		}
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func ValidateSocket(path string, expectedUID, expectedGID uint32, expectedMode os.FileMode) (Identity, error) {
	if _, err := ValidateParent(path, expectedUID); err != nil {
		return Identity{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Identity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || uint32(stat.Uid) != expectedUID || uint32(stat.Gid) != expectedGID || info.Mode().Perm() != expectedMode.Perm() {
		return Identity{}, errors.New("unix socket type, owner, group, or mode is invalid")
	}
	return Identity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func SameSocket(path string, expected Identity) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || uint64(stat.Dev) != expected.Device || stat.Ino != expected.Inode {
		return errors.New("unix socket pathname identity changed")
	}
	return nil
}

func RequirePeerUID(connection net.Conn, expectedUID uint32) error {
	actual, err := peerUID(connection)
	if err != nil {
		return fmt.Errorf("inspect Unix peer credentials: %w", err)
	}
	if actual != expectedUID {
		return fmt.Errorf("unix peer uid %d is not authorized", actual)
	}
	return nil
}
