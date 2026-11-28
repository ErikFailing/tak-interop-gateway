package latticesdk_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/cotgen"
	"github.com/erikfailing/tak-interop-gateway/internal/latticefake"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/publish/latticesdk"
	"github.com/erikfailing/tak-interop-gateway/readback"
)

// The tokens the fake requires. They're test values, not secrets.
const (
	testToken   = "test-environment-token"
	testSandbox = "test-sandboxes-token"
)

// The adapter is a Publisher, and a readback Client.
var (
	_ publish.Publisher = (*latticesdk.Client)(nil)
	_ readback.Client   = (*latticesdk.Client)(nil)
)

// now is the scenario's start.
var now = time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

// scenarioEntities maps one tick of a cot-gen scenario with the real parser and mapper.
func scenarioEntities(t *testing.T) []entity.Entity {
	t.Helper()
	s, err := cotgen.New(cotgen.Config{
		Units: 3, Origin: cotgen.LatLon{Lat: 40.3428, Lon: -105.6836}, Start: now,
		Interval: 5 * time.Second, Stale: 2 * time.Minute, Seed: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []entity.Entity
	for _, ev := range s.Next() {
		b, err := xml.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := cot.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		e, err := mapping.Map(parsed, mapping.Config{Source: "sdk-test", Simulated: true, TrackQuality: 2}, now)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// filled returns an entity with every model field set, so the conversion tests cover all of them.
func filled() entity.Entity {
	var e entity.Entity
	fill(reflect.ValueOf(&e).Elem())
	return e
}

// fill sets every field reachable from v to a non-zero value.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(now))
			return
		}
		for i := range v.NumField() {
			fill(v.Field(i))
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int:
		v.SetInt(3)
	case reflect.Float64:
		v.SetFloat(1.5)
	}
}

// TestSDKSendsWhatPreflightChecked converts entities to the SDK's type and back to JSON, and requires the SDK's JSON to
// match the model's field for field. Preflight checks the model's JSON, so this is what makes its verdict hold for the
// request the SDK actually sends.
func TestSDKSendsWhatPreflightChecked(t *testing.T) {
	for _, e := range append(scenarioEntities(t), filled()) {
		ours, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		sdkEntity, err := latticesdk.ToSDK(e)
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := json.Marshal(sdkEntity)
		if err != nil {
			t.Fatal(err)
		}
		diffs, err := readback.Diff(ours, theirs)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range diffs {
			t.Errorf("%s: the SDK's JSON differs from the model's: %s", *e.EntityID, d)
		}
	}
}

// newFake serves a latticefake.Server over plain http on loopback and returns an adapter pointed at it.
func newFake(t *testing.T, fake *latticefake.Server, cfg latticesdk.Config) *latticesdk.Client {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL
	c, err := latticesdk.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRoundTrip publishes through the SDK to the fake and reads back: everything sent comes back, plus the
// createdTime the server fills in.
func TestRoundTrip(t *testing.T) {
	fake := &latticefake.Server{Token: testToken, SandboxToken: testSandbox}
	c := newFake(t, fake, latticesdk.Config{Token: testToken, SandboxToken: testSandbox})
	results := readback.Run(context.Background(), c, scenarioEntities(t), 0)
	for _, r := range results {
		if !r.Passed() {
			t.Errorf("%s: err %v, differences %v", r.Name, r.Err, r.Differences)
		}
		if len(r.Differences) != 1 || r.Differences[0].Path != "createdTime" {
			t.Errorf("%s: want only the server's createdTime added, got %v", r.Name, r.Differences)
		}
	}
}

// TestErrors checks how each server answer is classified, and that no error message carries a token.
func TestErrors(t *testing.T) {
	ctx := context.Background()
	e := scenarioEntities(t)[0]
	tests := []struct {
		name string
		cfg  latticesdk.Config
		call func(c *latticesdk.Client) error
		want error
	}{
		{"wrong token", latticesdk.Config{Token: "wrong", SandboxToken: testSandbox},
			func(c *latticesdk.Client) error { return c.Publish(ctx, e) }, latticesdk.ErrUnauthorized},
		{"missing Sandboxes token", latticesdk.Config{Token: testToken},
			func(c *latticesdk.Client) error { return c.Publish(ctx, e) }, latticesdk.ErrUnauthorized},
		{"entity without an ID", latticesdk.Config{Token: testToken, SandboxToken: testSandbox},
			func(c *latticesdk.Client) error { return c.Publish(ctx, entity.Entity{IsLive: new(true)}) }, latticesdk.ErrBadRequest},
		{"unknown entity", latticesdk.Config{Token: testToken, SandboxToken: testSandbox},
			func(c *latticesdk.Client) error { _, err := c.Get(ctx, "no-such-entity"); return err }, latticesdk.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFake(t, &latticefake.Server{Token: testToken, SandboxToken: testSandbox}, tt.cfg)
			err := tt.call(c)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			for _, secret := range []string{testToken, testSandbox} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error message contains a token: %v", err)
				}
			}
		})
	}
}

// TestOtherStatus checks that a status the SDK has no type for is reported with its code. 418 is not retried, so
// the test doesn't wait out the SDK's backoff.
func TestOtherStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, `{"message":"no"}`)
	}))
	defer srv.Close()
	c, err := latticesdk.New(latticesdk.Config{BaseURL: srv.URL, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), scenarioEntities(t)[0]); err == nil || !strings.Contains(err.Error(), "HTTP 418") {
		t.Errorf("err = %v, want it to name HTTP 418", err)
	}
}

// TestHeaders checks exactly what reaches the server: the method, the path, and both credentials in the form
// Anduril's guide gives.
func TestHeaders(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c, err := latticesdk.New(latticesdk.Config{BaseURL: srv.URL, Token: testToken, SandboxToken: testSandbox})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), scenarioEntities(t)[0]); err != nil {
		t.Fatal(err)
	}
	switch {
	case got.Method != http.MethodPut || got.URL.Path != "/api/v1/entities":
		t.Errorf("request = %s %s, want PUT /api/v1/entities", got.Method, got.URL.Path)
	case got.Header.Get("Authorization") != "Bearer "+testToken:
		t.Errorf("Authorization = %q", got.Header.Get("Authorization"))
	case got.Header.Get(latticesdk.SandboxHeader) != "Bearer "+testSandbox:
		t.Errorf("%s = %q", latticesdk.SandboxHeader, got.Header.Get(latticesdk.SandboxHeader))
	}
}

// TestConfigFromEnv checks the environment variables and what's wrong with each bad set.
func TestConfigFromEnv(t *testing.T) {
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	cfg, err := latticesdk.ConfigFromEnv(env(map[string]string{
		"LATTICE_ENDPOINT": "abc.env.sandboxes.developer.anduril.com", "ENVIRONMENT_TOKEN": "t", "SANDBOXES_TOKEN": "s",
	}))
	if err != nil || cfg.Endpoint != "abc.env.sandboxes.developer.anduril.com" || cfg.Token != "t" || cfg.SandboxToken != "s" {
		t.Errorf("ConfigFromEnv = %v, %v", cfg, err)
	}
	for name, vars := range map[string]map[string]string{
		"no endpoint": {"ENVIRONMENT_TOKEN": "t"},
		"no token":    {"LATTICE_ENDPOINT": "abc.example.com"},
	} {
		if _, err := latticesdk.ConfigFromEnv(env(vars)); !errors.Is(err, latticesdk.ErrConfig) {
			t.Errorf("%s: err %v, want ErrConfig", name, err)
		}
	}
}

// TestNewChecksConfig checks the base URL rules: a bare hostname, or https, or plain http only to loopback.
func TestNewChecksConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  latticesdk.Config
		ok   bool
	}{
		{"hostname", latticesdk.Config{Endpoint: "abc.example.com", Token: "t"}, true},
		{"hostname and port", latticesdk.Config{Endpoint: "abc.example.com:8443", Token: "t"}, true},
		{"scheme in the endpoint", latticesdk.Config{Endpoint: "https://abc.example.com", Token: "t"}, false},
		{"path in the endpoint", latticesdk.Config{Endpoint: "abc.example.com/api", Token: "t"}, false},
		{"https base URL", latticesdk.Config{BaseURL: "https://abc.example.com", Token: "t"}, true},
		{"http to loopback", latticesdk.Config{BaseURL: "http://127.0.0.1:8080", Token: "t"}, true},
		{"http to localhost", latticesdk.Config{BaseURL: "http://localhost:8080", Token: "t"}, true},
		{"http beyond loopback", latticesdk.Config{BaseURL: "http://abc.example.com", Token: "t"}, false},
		{"base URL with a path", latticesdk.Config{BaseURL: "https://abc.example.com/x", Token: "t"}, false},
		{"other scheme", latticesdk.Config{BaseURL: "ftp://127.0.0.1", Token: "t"}, false},
		{"both", latticesdk.Config{Endpoint: "abc.example.com", BaseURL: "https://abc.example.com", Token: "t"}, false},
		{"neither", latticesdk.Config{Token: "t"}, false},
		{"no token", latticesdk.Config{Endpoint: "abc.example.com"}, false},
		{"base URL with a user and password", latticesdk.Config{BaseURL: "https://user:hunter2@abc.example.com", Token: "t"}, false},
		{"base URL with a query", latticesdk.Config{BaseURL: "https://abc.example.com?x=1", Token: "t"}, false},
		{"base URL with an empty query", latticesdk.Config{BaseURL: "https://abc.example.com?", Token: "t"}, false},
		{"base URL with a fragment", latticesdk.Config{BaseURL: "https://abc.example.com#x", Token: "t"}, false},
	}
	for _, tt := range tests {
		_, err := latticesdk.New(tt.cfg)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err %v, want ok=%v", tt.name, err, tt.ok)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the error repeats the password: %v", tt.name, err)
		}
	}
}

// TestNoRedirects checks that the adapter never follows a redirect, which would carry both tokens to a host the
// config never named, and that the caller's own HTTP client is left as it was.
func TestNoRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	own := &http.Client{}
	c, err := latticesdk.New(latticesdk.Config{BaseURL: srv.URL, Token: testToken, SandboxToken: testSandbox, HTTPClient: own})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), scenarioEntities(t)[0]); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Errorf("err = %v, want the 307 reported as an error", err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirect was followed: the other host got %d requests", n)
	}
	if own.CheckRedirect != nil {
		t.Error("New changed the caller's HTTP client")
	}
}

// TestStatusClassification checks that errors are classified by HTTP status, whatever the body: the SDK returns no
// typed error for a body it can't decode, such as a proxy's HTML page, and a refused token must still read as one.
func TestStatusClassification(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		get         bool
		want        error
	}{
		{"401 as an HTML page", http.StatusUnauthorized, "text/html", "<html>denied</html>", false, latticesdk.ErrUnauthorized},
		{"401 as JSON", http.StatusUnauthorized, "application/json", `{"message":"no"}`, false, latticesdk.ErrUnauthorized},
		{"403", http.StatusForbidden, "application/json", `{"message":"forbidden"}`, false, latticesdk.ErrUnauthorized},
		{"400 as plain text", http.StatusBadRequest, "text/plain", "bad entity", false, latticesdk.ErrBadRequest},
		{"404 as an HTML page", http.StatusNotFound, "text/html", "<html>gone</html>", true, latticesdk.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			c, err := latticesdk.New(latticesdk.Config{BaseURL: srv.URL, Token: testToken})
			if err != nil {
				t.Fatal(err)
			}
			if tt.get {
				_, err = c.Get(context.Background(), "some-id")
			} else {
				err = c.Publish(context.Background(), scenarioEntities(t)[0])
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestConfigRedactsSecrets checks that printing a config, by any verb, never prints a token.
func TestConfigRedactsSecrets(t *testing.T) {
	cfg := latticesdk.Config{Endpoint: "abc.example.com", Token: "secret-env-token", SandboxToken: "secret-sandbox-token"}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(format, cfg)
		if strings.Contains(out, "secret") {
			t.Errorf("%s printed a token: %s", format, out)
		}
	}
}
