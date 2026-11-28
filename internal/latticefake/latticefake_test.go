package latticefake_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/internal/latticefake"
)

// call sends one request with the given credentials and returns the status and the decoded JSON body.
func call(t *testing.T, srv *httptest.Server, method, path, token, sandbox, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if sandbox != "" {
		req.Header.Set("anduril-sandbox-authorization", "Bearer "+sandbox)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("response is not JSON: %q", b)
	}
	return resp.StatusCode, out
}

// TestFake checks the fake's routes, authentication and the one server behavior it adds.
func TestFake(t *testing.T) {
	first := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	clock := first
	fake := &latticefake.Server{Token: "t", SandboxToken: "s", Now: func() time.Time { return clock }}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	// Authentication: both tokens, each with its Bearer prefix.
	if code, _ := call(t, srv, http.MethodGet, "/api/v1/entities/a", "wrong", "s", ""); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/api/v1/entities/a", "t", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no Sandboxes token: status %d", code)
	}

	// Publish fills in createdTime, and a republish keeps the first one.
	if code, e := call(t, srv, http.MethodPut, "/api/v1/entities", "t", "s", `{"entityId":"a","isLive":true}`); code != http.StatusOK || e["createdTime"] != "2026-11-27T16:00:00Z" {
		t.Errorf("publish: status %d, entity %v", code, e)
	}
	clock = first.Add(time.Minute)
	call(t, srv, http.MethodPut, "/api/v1/entities", "t", "s", `{"entityId":"a","isLive":true,"aliases":{"name":"x"}}`)
	code, e := call(t, srv, http.MethodGet, "/api/v1/entities/a", "t", "s", "")
	if code != http.StatusOK || e["createdTime"] != "2026-11-27T16:00:00Z" || e["aliases"] == nil {
		t.Errorf("get after republish: status %d, entity %v", code, e)
	}

	// Refusals.
	for _, tt := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"no entityId", http.MethodPut, "/api/v1/entities", `{"isLive":true}`, http.StatusBadRequest},
		{"not an object", http.MethodPut, "/api/v1/entities", `[1]`, http.StatusBadRequest},
		{"unknown entity", http.MethodGet, "/api/v1/entities/missing", "", http.StatusNotFound},
		{"unknown route", http.MethodGet, "/api/v1/tasks", "", http.StatusNotFound},
		{"wrong method", http.MethodDelete, "/api/v1/entities", "", http.StatusNotFound},
	} {
		if code, _ := call(t, srv, tt.method, tt.path, "t", "s", tt.body); code != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, code, tt.want)
		}
	}
	if fake.Requests() == 0 {
		t.Error("Requests counted nothing")
	}
}

// TestMutate checks that the mutation hook changes what is stored, which is how tests stand in for a platform that
// drops a field.
func TestMutate(t *testing.T) {
	fake := &latticefake.Server{Token: "t", Mutate: func(e map[string]any) { delete(e, "isLive") }}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	call(t, srv, http.MethodPut, "/api/v1/entities", "t", "", `{"entityId":"a","isLive":true}`)
	if _, e := call(t, srv, http.MethodGet, "/api/v1/entities/a", "t", "", ""); e["isLive"] != nil {
		t.Errorf("the mutation didn't apply: %v", e)
	}
}
