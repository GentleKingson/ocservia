package certificates

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHTTPSignerPublicKey(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	status, body := 200, `{"node_id":"`+id.String()+`"}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		if r.Method != "POST" || r.URL.Path != "/sign/public-key" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&request) != nil || request["node_id"] != id.String() || request["purpose"] != "user_password" {
			t.Error("incorrect authenticated public key request")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	signer, err := NewHTTPSigner(server.URL+"/sign", "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	signer.client = server.Client()
	data, err := signer.UserPasswordPublicKey(t.Context(), id)
	if err != nil || string(data) != body {
		t.Fatalf("response: %q %v", data, err)
	}
	status = 403
	if _, err := signer.UserPasswordPublicKey(t.Context(), id); err == nil {
		t.Fatal("rejected binding accepted")
	}
	status, body = 200, strings.Repeat("x", (16<<10)+1)
	if _, err := signer.UserPasswordPublicKey(t.Context(), id); err == nil {
		t.Fatal("unbounded response accepted")
	}
}
