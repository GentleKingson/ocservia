package certificates

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	agentv1 "github.com/GentleKingson/ocservia/control-plane/gen/proto/ocserv/platform/agent/v1"
	"github.com/google/uuid"
)

const maxSignerResponseBytes = 512 << 10

type HTTPSigner struct {
	endpoint string
	token    string
	client   *http.Client
}

func NewHTTPSigner(endpoint, token string, timeout time.Duration) (*HTTPSigner, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimSpace(token) == "" || timeout < time.Second || timeout > 30*time.Second {
		return nil, errors.New("external certificate signer configuration is invalid")
	}
	return &HTTPSigner{endpoint: parsed.String(), token: token, client: &http.Client{Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("external signer redirects are forbidden")
	}}}, nil
}

func (s *HTTPSigner) Sign(ctx context.Context, request SignRequest) (SignResult, error) {
	body, err := json.Marshal(map[string]string{"certificate_id": request.CertificateID.String(), "csr_der": base64.StdEncoding.EncodeToString(request.CSRDER)})
	if err != nil {
		return SignResult{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return SignResult{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+s.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.CertificateID.String())
	response, err := s.client.Do(httpRequest)
	if err != nil {
		return SignResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return SignResult{}, errors.New("external signer rejected the request")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSignerResponseBytes+1))
	if err != nil || len(data) > maxSignerResponseBytes {
		return SignResult{}, errors.New("external signer response is invalid")
	}
	var decoded struct {
		CertificateChainPEM string `json:"certificate_chain_pem"`
	}
	if json.Unmarshal(data, &decoded) != nil || len(decoded.CertificateChainPEM) == 0 {
		return SignResult{}, errors.New("external signer response is invalid")
	}
	return SignResult{CertificateChainPEM: []byte(decoded.CertificateChainPEM)}, nil
}

func (s *HTTPSigner) Revoke(ctx context.Context, request RevokeSignerRequest) error {
	body, err := json.Marshal(map[string]string{"certificate_id": request.CertificateID.String(), "serial_number": request.SerialNumber, "reason": request.Reason})
	if err != nil {
		return err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.endpoint, "/")+"/revoke", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+s.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.CertificateID.String()+":revoke")
	response, err := s.client.Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return errors.New("external signer rejected revocation")
	}
	return nil
}

func (s *HTTPSigner) Seal(ctx context.Context, nodeID uuid.UUID, purpose agentv1.SealedSecretPurpose, plaintext []byte) (*agentv1.SealedSecretV1, error) {
	if purpose != agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD && purpose != agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD {
		return nil, errors.New("secret sealing purpose is invalid")
	}
	body := append([]byte(nil), plaintext...)
	defer func() {
		for index := range body {
			body[index] = 0
		}
	}()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.endpoint, "/")+"/seal", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+s.token)
	httpRequest.Header.Set("Content-Type", "application/octet-stream")
	httpRequest.Header.Set("X-Ocservia-Node-ID", nodeID.String())
	purposeName := sealPurposeName(purpose)
	httpRequest.Header.Set("X-Ocservia-Seal-Purpose", purposeName)
	response, err := s.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 20*1024))
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, errors.New("external secret provider rejected sealing")
	}
	var decoded struct {
		Sealed  string `json:"sealed"`
		KeyID   string `json:"key_id"`
		Version uint32 `json:"version"`
		Purpose string `json:"purpose"`
	}
	if json.Unmarshal(data, &decoded) != nil || decoded.Version != 1 || decoded.Purpose != purposeName || decoded.KeyID == "" || len(decoded.KeyID) > 128 {
		return nil, errors.New("external secret provider response is invalid")
	}
	sealed, err := base64.StdEncoding.DecodeString(decoded.Sealed)
	if err != nil || len(sealed) < 32 || len(sealed) > 16*1024 {
		return nil, errors.New("external secret provider response is invalid")
	}
	return &agentv1.SealedSecretV1{Version: agentv1.SealedSecretVersion_SEALED_SECRET_VERSION_V1, Purpose: purpose, KeyId: decoded.KeyID, Ciphertext: sealed}, nil
}

func sealPurposeName(purpose agentv1.SealedSecretPurpose) string {
	if purpose == agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_USER_PASSWORD {
		return "user_password"
	}
	if purpose == agentv1.SealedSecretPurpose_SEALED_SECRET_PURPOSE_CERTIFICATE_P12_PASSWORD {
		return "certificate_p12_password"
	}
	return ""
}
