package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifiedPublicKeyRead(t *testing.T) {
	s, dir := fixture(t)
	b, _ := approvedKeys(t)
	body, _ := json.Marshal(map[string]string{"node_id": b.NodeID, "purpose": "user_password"})
	if w := request(s, "/sign/public-key", body, nil); w.Code != 403 {
		t.Fatal("unbound public key returned", w.Code)
	}
	must(t, s.importBinding(b, b))
	w := request(s, "/sign/public-key", body, nil)
	var value map[string]json.RawMessage
	must(t, json.Unmarshal(w.Body.Bytes(), &value))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || string(value["workspace_id"]) != `"`+b.WorkspaceID+`"` || string(value["node_id"]) != `"`+b.NodeID+`"` || string(value["endpoint_id"]) != `"`+b.EndpointID+`"` || string(value["key_id"]) != `"`+b.Keys[0].KeyID+`"` || string(value["public_key_sha256"]) != `"`+b.Keys[0].Digest+`"` {
		t.Fatal("incorrect public binding", w.Code)
	}
	var der []byte
	must(t, json.Unmarshal(value["public_key_der"], &der))
	if digest(der) != b.Keys[0].Digest || strings.Contains(w.Body.String(), "private") {
		t.Fatal("incorrect key material")
	}
	for _, tc := range []struct {
		body    []byte
		headers map[string]string
		code    int
	}{
		{body, map[string]string{"Authorization": "Bearer wrong"}, 401},
		{[]byte(`{"node_id":"` + b.NodeID + `","purpose":"certificate_p12_password"}`), nil, 400},
		{append(body, []byte(`{}`)...), nil, 400},
		{[]byte(`{"node_id":"` + b.NodeID + `","purpose":"user_password","extra":true}`), nil, 400},
	} {
		if got := request(s, "/sign/public-key", tc.body, tc.headers).Code; got != tc.code {
			t.Fatalf("got %d want %d", got, tc.code)
		}
	}
	must(t, s.disable(b.NodeID))
	if request(s, "/sign/public-key", body, nil).Code != 403 {
		t.Fatal("disabled binding served")
	}
	must(t, s.db.Close())
	restarted, err := openStore(dir+"/ledger.db", s.ca, false)
	must(t, err)
	defer restarted.db.Close()
	if _, err := restarted.userPasswordPublicKey(b.NodeID); err != statusError(403) {
		t.Fatal("disabled binding served after restart")
	}
}
