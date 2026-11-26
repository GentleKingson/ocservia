package main

import (
	"fmt"
	"os"

	"github.com/GentleKingson/ocservia/control-plane/internal/protectedfile"
)

func readCredentialFile(path string) ([]byte, error) {
	raw, err := protectedfile.ReadPrivate(path, uint32(os.Geteuid()), true, 4096)
	if err != nil {
		return nil, fmt.Errorf("read credential: %w", err)
	}
	return raw, nil
}

func openCredentialFile(path string) (*os.File, error) {
	file, err := protectedfile.OpenPrivate(path, uint32(os.Geteuid()), true, 4096)
	if err != nil {
		return nil, fmt.Errorf("open credential: %w", err)
	}
	return file, nil
}
