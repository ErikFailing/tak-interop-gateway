// Package latticefake is an in-process stand-in for the two Lattice REST endpoints the gateway uses: publish
// (PUT /api/v1/entities) and get (GET /api/v1/entities/{id}). Tests and `takgw readback`'s offline mode point the real
// SDK adapter at it, so the whole path, authentication headers included, runs with no Lattice environment.
//
// It is deliberately simple. It checks the bearer token and, when configured, the Sandboxes token; it requires an
// entityId; and it stores what it's given. The one server behavior it adds is the documented one: createdTime is set
// to the time the entity was first received when the publisher leaves it empty. It does not validate entities the way
// the platform does, because that's Entity Preflight's job, and only a live readback can show what the platform
// really does.
package latticefake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxBody bounds a request body.
const maxBody = 1 << 20

// Server is the fake. Set its fields before serving; it is safe for concurrent requests.
type Server struct {
	Token        string           // the bearer token to require
	SandboxToken string           // when set, the Sandboxes token to require in anduril-sandbox-authorization
	Now          func() time.Time // the clock for createdTime; nil means time.Now
	// Mutate, when set, changes each entity before it's stored, to make the fake behave like a platform that drops
	// or rewrites fields, so tests can prove readback notices.
	Mutate func(entity map[string]any)

	mu       sync.Mutex
	entities map[string]map[string]any
	requests int
}

// Requests returns how many requests have passed the authentication check.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1: Authenticate both tokens, as a Sandbox does.
	if r.Header.Get("Authorization") != "Bearer "+s.Token ||
		(s.SandboxToken != "" && r.Header.Get("anduril-sandbox-authorization") != "Bearer "+s.SandboxToken) {
		reply(w, http.StatusUnauthorized, map[string]any{"message": "unauthorized"})
		return
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()

	// 2: Route the two endpoints.
	const prefix = "/api/v1/entities"
	switch {
	case r.URL.Path == prefix && r.Method == http.MethodPut:
		s.publish(w, r)
	case strings.HasPrefix(r.URL.Path, prefix+"/") && r.Method == http.MethodGet:
		s.get(w, strings.TrimPrefix(r.URL.Path, prefix+"/"))
	default:
		reply(w, http.StatusNotFound, map[string]any{"message": "no such endpoint"})
	}
}

// publish stores one entity and echoes it back, as the platform does.
func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	// 1: Read one JSON object.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		reply(w, http.StatusBadRequest, map[string]any{"message": "body unreadable or too large"})
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var e map[string]any
	if err := dec.Decode(&e); err != nil || e == nil {
		reply(w, http.StatusBadRequest, map[string]any{"message": "body is not a JSON object"})
		return
	}
	id, _ := e["entityId"].(string)
	if id == "" {
		reply(w, http.StatusBadRequest, map[string]any{"message": "entityId must be set"})
		return
	}

	// 2: Keep the first createdTime, or set it now, then apply any test mutation and store.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entities == nil {
		s.entities = map[string]map[string]any{}
	}
	if _, set := e["createdTime"]; !set {
		if prev, ok := s.entities[id]["createdTime"]; ok {
			e["createdTime"] = prev
		} else {
			now := time.Now
			if s.Now != nil {
				now = s.Now
			}
			e["createdTime"] = now().UTC().Format(time.RFC3339Nano)
		}
	}
	if s.Mutate != nil {
		s.Mutate(e)
	}
	s.entities[id] = e
	reply(w, http.StatusOK, e)
}

// get returns one stored entity.
func (s *Server) get(w http.ResponseWriter, id string) {
	s.mu.Lock()
	e, ok := s.entities[id]
	s.mu.Unlock()
	if !ok {
		reply(w, http.StatusNotFound, map[string]any{"message": "entity not found"})
		return
	}
	reply(w, http.StatusOK, e)
}

// reply writes a JSON response.
func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client hung up if this fails; there's no one left to tell
}
