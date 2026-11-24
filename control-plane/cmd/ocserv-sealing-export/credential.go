package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/GentleKingson/ocservia/control-plane/internal/protectedfile"
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

func openCredentialFile(path string) (*os.File, error) {
	uid := uint32(os.Geteuid())
	file, err := protectedfile.Open(path, uid)
	if err != nil {
		return nil, fmt.Errorf("open credential: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
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
