package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeStrictJSONUTF8(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		valid            bool
	}{
		{"unicode", "{\"value\":\" \u754c\ufffd \"}", " \u754c\ufffd ", true},
		{"escaped-unicode", `{"value":"\u754c\ufffd"}`, "\u754c\ufffd", true},
		{"invalid-byte", "{\"value\":\"\xff\"}", "", false},
		{"truncated-sequence", "{\"value\":\"\xe7\x95\"}", "", false},
		{"unknown-field", `{"other":"value"}`, "", false},
		{"trailing-value", `{"value":"ok"}{}`, "", false},
		{"trailing-invalid-byte", "{\"value\":\"ok\"}\xff", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			var target struct {
				Value string `json:"value"`
			}
			if got := decodeStrictJSON(w, r, &target); got != tc.valid || tc.valid && target.Value != tc.want {
				t.Fatalf("decode result=%v value=%q", got, target.Value)
			}
			if !tc.valid && w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
}

func TestDecodeStrictJSONBodyLimit(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"value":"too long"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(w, r.Body, 8)
	var target struct {
		Value string `json:"value"`
	}
	if decodeStrictJSON(w, r, &target) || w.Code != http.StatusBadRequest || target.Value != "" {
		t.Fatal("oversized body was decoded")
	}
}
