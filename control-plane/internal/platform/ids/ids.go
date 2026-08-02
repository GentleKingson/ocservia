package ids

import (
	"fmt"

	"github.com/google/uuid"
)

type Generator interface{ New() (uuid.UUID, error) }

type UUIDv7 struct{}

func (UUIDv7) New() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate UUIDv7: %w", err)
	}
	return id, nil
}
