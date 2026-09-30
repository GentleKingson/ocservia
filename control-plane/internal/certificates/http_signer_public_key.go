package certificates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

func (s *HTTPSigner) UserPasswordPublicKey(ctx context.Context, nodeID uuid.UUID) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"node_id": nodeID.String(), "purpose": "user_password"})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.endpoint, "/")+"/public-key", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<10)+1))
	if err != nil || response.StatusCode != http.StatusOK || len(data) > 16<<10 {
		return nil, errors.New("verified sealing public key is unavailable")
	}
	return data, nil
}
