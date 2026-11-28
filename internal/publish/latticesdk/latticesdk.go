// Package latticesdk publishes entities to Lattice through the official Go SDK. It is the only package, besides
// Entity Preflight, allowed to import the SDK, and it keeps the SDK at arm's length: the rest of the gateway passes
// the SDK-free entity model, and this adapter converts it at the last moment.
//
// The conversion goes through JSON. The entity model's field names are the REST API's, and a test in the preflight
// package proves that every one of them is a field the SDK declares, so marshaling the model and unmarshaling it into
// the SDK's Entity carries every field across with no hand-written mapping to drift. A test here checks the other
// direction: the SDK sends exactly the JSON that preflight checked.
//
// Credentials come only from the environment, and are never logged or printed.
package latticesdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	lattice "github.com/anduril/lattice-sdk-go/v5"
	"github.com/anduril/lattice-sdk-go/v5/client"
	"github.com/anduril/lattice-sdk-go/v5/core"
	"github.com/anduril/lattice-sdk-go/v5/option"

	"github.com/erikfailing/tak-interop-gateway/entity"
)

// Errors the adapter returns. Each wraps the SDK's error, whose text is the server's response.
var (
	ErrConfig       = errors.New("latticesdk: invalid config")
	ErrRejected     = errors.New("latticesdk: the server rejected the entity")
	ErrUnauthorized = errors.New("latticesdk: the server refused the credentials")
	ErrNotFound     = errors.New("latticesdk: no such entity")
)

// SandboxHeader carries the Sandboxes token, in addition to the environment token, on every request.
const SandboxHeader = "anduril-sandbox-authorization"

// defaultTimeout bounds each HTTP request, so a stalled server can't hang the gateway.
const defaultTimeout = 30 * time.Second

// hostname matches a DNS name, optionally with a port: what LATTICE_ENDPOINT must hold.
var hostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// Config says how to reach one Lattice environment.
type Config struct {
	// Endpoint is the environment's hostname, with no scheme (LATTICE_ENDPOINT). Requests go to https://Endpoint.
	Endpoint string
	// BaseURL replaces Endpoint for a server that isn't a real environment, such as the in-process fake. It must be
	// https, or plain http to a loopback address.
	BaseURL string
	// Token is the environment's bearer token (ENVIRONMENT_TOKEN).
	Token string
	// SandboxToken is the account's Sandboxes token (SANDBOXES_TOKEN), sent in SandboxHeader. Leave it empty outside
	// Sandboxes.
	SandboxToken string
	// HTTPClient sends the requests; nil means one with a 30-second timeout.
	HTTPClient *http.Client
}

// String describes the config without its secrets, so a config printed by mistake leaks nothing.
func (c Config) String() string {
	return fmt.Sprintf("latticesdk.Config{Endpoint: %q, BaseURL: %q, Token set: %t, SandboxToken set: %t}",
		c.Endpoint, c.BaseURL, c.Token != "", c.SandboxToken != "")
}

// GoString is String, so %#v redacts too.
func (c Config) GoString() string {
	return c.String()
}

// ConfigFromEnv reads LATTICE_ENDPOINT, ENVIRONMENT_TOKEN and SANDBOXES_TOKEN, the variable names Anduril's
// documentation uses. getenv is os.Getenv outside tests.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Endpoint:     strings.TrimSpace(getenv("LATTICE_ENDPOINT")),
		Token:        strings.TrimSpace(getenv("ENVIRONMENT_TOKEN")),
		SandboxToken: strings.TrimSpace(getenv("SANDBOXES_TOKEN")),
	}
	switch {
	case cfg.Endpoint == "":
		return Config{}, fmt.Errorf("%w: LATTICE_ENDPOINT is not set", ErrConfig)
	case cfg.Token == "":
		return Config{}, fmt.Errorf("%w: ENVIRONMENT_TOKEN is not set", ErrConfig)
	}
	return cfg, nil
}

// Client publishes entities to, and reads them from, one Lattice environment. It is safe for concurrent use.
type Client struct {
	sdk *client.Client
}

// New checks the config and builds a client. It sends nothing: the first request is the first proof the credentials
// work.
func New(cfg Config) (*Client, error) {
	// 1: Work out the base URL, refusing plain http anywhere but loopback.
	base, err := baseURL(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("%w: no token", ErrConfig)
	}

	// 2: The static token goes in the Authorization header on every request; set that way, it takes precedence over
	// the SDK's OAuth client-credentials flow, which then never runs. The Sandboxes token rides along on the client
	// itself, as Anduril's guide says, so it covers every request.
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	header := http.Header{}
	if cfg.SandboxToken != "" {
		header.Set(SandboxHeader, "Bearer "+cfg.SandboxToken)
	}
	sdk := client.NewClient(
		option.WithBaseURL(base),
		option.WithToken(cfg.Token),
		option.WithHTTPHeader(header),
		option.WithHTTPClient(httpClient),
	)
	return &Client{sdk: sdk}, nil
}

// baseURL returns https://Endpoint, or a checked BaseURL.
func baseURL(cfg Config) (string, error) {
	switch {
	case (cfg.Endpoint == "") == (cfg.BaseURL == ""):
		return "", fmt.Errorf("%w: set exactly one of Endpoint and BaseURL", ErrConfig)
	case cfg.Endpoint != "":
		if strings.Contains(cfg.Endpoint, "://") {
			return "", fmt.Errorf("%w: LATTICE_ENDPOINT is a hostname; drop the scheme", ErrConfig)
		}
		if !hostname.MatchString(cfg.Endpoint) {
			return "", fmt.Errorf("%w: LATTICE_ENDPOINT %q is not a hostname", ErrConfig, cfg.Endpoint)
		}
		return "https://" + cfg.Endpoint, nil
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%w: BaseURL %q is not a scheme and host", ErrConfig, cfg.BaseURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("%w: plain http is only allowed to a loopback address, not %q", ErrConfig, host)
		}
	default:
		return "", fmt.Errorf("%w: BaseURL scheme must be https or http", ErrConfig)
	}
	return strings.TrimSuffix(cfg.BaseURL, "/"), nil
}

// Publish sends one entity. It implements publish.Publisher.
func (c *Client) Publish(ctx context.Context, e entity.Entity) error {
	sdkEntity, err := ToSDK(e)
	if err != nil {
		return err
	}
	if _, err := c.sdk.Entities.PublishEntity(ctx, sdkEntity); err != nil {
		return classify("publish", err)
	}
	return nil
}

// Get reads one entity back, as JSON in the REST API's shape. The JSON is the SDK's own encoding of what it decoded,
// so it holds every field this SDK version knows, including ones the gateway never set, such as createdTime.
func (c *Client) Get(ctx context.Context, id string) ([]byte, error) {
	got, err := c.sdk.Entities.GetEntity(ctx, &lattice.GetEntityRequest{EntityID: id})
	if err != nil {
		return nil, classify("get", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		return nil, fmt.Errorf("latticesdk: encode the entity read back: %w", err)
	}
	return b, nil
}

// ToSDK converts an entity to the SDK's type, through its JSON form.
func ToSDK(e entity.Entity) (*lattice.Entity, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("latticesdk: encode entity: %w", err)
	}
	var out lattice.Entity
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("latticesdk: convert entity: %w", err)
	}
	return &out, nil
}

// classify wraps an SDK error with the adapter's sentinel for its status, so callers can decide what's fatal: a
// refused credential will fail every request, a rejected entity only that one.
func classify(op string, err error) error {
	var (
		bad          *lattice.BadRequestError
		unauthorized *lattice.UnauthorizedError
		notFound     *lattice.NotFoundError
		apiErr       *core.APIError
	)
	switch {
	case errors.As(err, &bad):
		return fmt.Errorf("%w (%s): %w", ErrRejected, op, err)
	case errors.As(err, &unauthorized):
		return fmt.Errorf("%w (%s): %w", ErrUnauthorized, op, err)
	case errors.As(err, &notFound):
		return fmt.Errorf("%w (%s): %w", ErrNotFound, op, err)
	case errors.As(err, &apiErr):
		return fmt.Errorf("latticesdk: %s: HTTP %d: %w", op, apiErr.StatusCode, err)
	}
	return fmt.Errorf("latticesdk: %s: %w", op, err)
}
