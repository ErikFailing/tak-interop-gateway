package health_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/health"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// now is when the tests take their snapshots.
var now = time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)

// cfg is a valid config.
var cfg = health.Config{Source: "tak-main", Latitude: 40.3428, Longitude: -105.6836, Interval: 30 * time.Second, Input: "TAK server tak.example.org:8089"}

// newMonitor returns a monitor for cfg, failing the test on error.
func newMonitor(t *testing.T) *health.Monitor {
	t.Helper()
	m, err := health.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// up is a healthy input.
var up = health.Input{Up: true, Detail: "connected to tak.example.org:8089"}

// levels returns the roll-up and each component's level, input, integrity, output, and the sensor's state.
func levels(e entity.Entity) (overall string, components []string, sensor string) {
	for _, c := range e.Health.Components {
		components = append(components, string(*c.Health))
	}
	return string(*e.Health.HealthStatus), components, string(*e.Sensors.Sensors[0].OperationalState)
}

// TestStatus checks the roll-up for each kind of window.
func TestStatus(t *testing.T) {
	tests := []struct {
		name       string
		totals     health.Totals
		in         health.Input
		overall    entity.HealthStatus
		components []entity.HealthStatus // input, integrity, output
		sensor     entity.OperationalState
		message    string // a substring of the integrity or output message, when set
	}{
		{"quiet", health.Totals{}, up, entity.HealthHealthy,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthHealthy, entity.HealthHealthy}, entity.StateOperational, "no suspect input"},
		{"busy and clean", health.Totals{Messages: 400, Published: 390}, up, entity.HealthHealthy,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthHealthy, entity.HealthHealthy}, entity.StateOperational, "390 published"},
		{"one implausible jump", health.Totals{Messages: 400, Published: 390, Implausible: 1}, up, entity.HealthWarn,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthWarn, entity.HealthHealthy}, entity.StateDegraded, "1 implausible jump"},
		{"an origin conflict", health.Totals{Messages: 10, OriginConflicts: 2}, up, entity.HealthWarn,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthWarn, entity.HealthHealthy}, entity.StateDegraded, "2 reports from another origin"},
		{"a few bad messages in a busy window", health.Totals{Messages: 400, Unparsable: 6}, up, entity.HealthHealthy,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthHealthy, entity.HealthHealthy}, entity.StateOperational, ""},
		{"a spike of bad messages", health.Totals{Messages: 40, Unparsable: 4, Rejected: 1}, up, entity.HealthWarn,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthWarn, entity.HealthHealthy}, entity.StateDegraded, "4 unparsable messages"},
		{"one bad message in a quiet window", health.Totals{Messages: 1, Unparsable: 1}, up, entity.HealthHealthy,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthHealthy, entity.HealthHealthy}, entity.StateOperational, ""},
		{"publishes failing", health.Totals{Messages: 10, Published: 7, PublishErrors: 3}, up, entity.HealthFail,
			[]entity.HealthStatus{entity.HealthHealthy, entity.HealthHealthy, entity.HealthFail}, entity.StateOperational, "3 of 10 publishes failed"},
		{"input down", health.Totals{}, health.Input{Detail: "not connected"}, entity.HealthFail,
			[]entity.HealthStatus{entity.HealthFail, entity.HealthHealthy, entity.HealthHealthy}, entity.StateNonOperational, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newMonitor(t).Snapshot(now, tt.totals, tt.in)
			overall, components, sensor := levels(e)
			var want []string
			for _, c := range tt.components {
				want = append(want, string(c))
			}
			if overall != string(tt.overall) || strings.Join(components, ",") != strings.Join(want, ",") || sensor != string(tt.sensor) {
				t.Errorf("got %s %v %s, want %s %v %s", overall, components, sensor, tt.overall, want, tt.sensor)
			}
			if tt.message != "" {
				all := *e.Health.Components[1].Messages[0].Message + " | " + *e.Health.Components[2].Messages[0].Message
				if !strings.Contains(all, tt.message) {
					t.Errorf("messages %q, want %q", all, tt.message)
				}
			}
		})
	}
}

// TestWindow checks that each snapshot counts only what happened since the previous one.
func TestWindow(t *testing.T) {
	m := newMonitor(t)
	if overall, _, _ := levels(m.Snapshot(now, health.Totals{Messages: 10, Implausible: 1}, up)); overall != string(entity.HealthWarn) {
		t.Fatalf("first window: %s, want WARN", overall)
	}
	if overall, _, _ := levels(m.Snapshot(now.Add(30*time.Second), health.Totals{Messages: 50, Implausible: 1}, up)); overall != string(entity.HealthHealthy) {
		t.Errorf("second window, with no new jumps: %s, want HEALTHY", overall)
	}
}

// TestAssetPassesPreflight checks that every form of the asset passes preflight with no finding at all, as a value
// and as JSON, and that it is the documented shape: a live friendly ground asset that expires after three intervals.
func TestAssetPassesPreflight(t *testing.T) {
	sim := cfg
	sim.Simulated = true
	ms, err := health.New(sim)
	if err != nil {
		t.Fatal(err)
	}
	alt := cfg
	alt.Altitude = new(1650.0)
	ma, err := health.New(alt)
	if err != nil {
		t.Fatal(err)
	}
	m := newMonitor(t)
	for name, e := range map[string]entity.Entity{
		"healthy":       m.Snapshot(now, health.Totals{}, up),
		"warning":       m.Snapshot(now, health.Totals{Messages: 10, Implausible: 3}, up),
		"failing":       m.Snapshot(now, health.Totals{PublishErrors: 1}, health.Input{Detail: "down"}),
		"offline":       m.Offline(now),
		"simulated":     ms.Snapshot(now, health.Totals{}, up),
		"with altitude": ma.Snapshot(now, health.Totals{}, up),
	} {
		check := preflight.NewChecker(func() time.Time { return now })
		if vs := check.Check(e); vs != nil {
			t.Errorf("%s: %v", name, vs)
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if vs, err := preflight.NewChecker(func() time.Time { return now }).CheckJSON(b); err != nil || vs != nil {
			t.Errorf("%s as JSON: %v %v", name, vs, err)
		}
		if *e.Ontology.Template != entity.TemplateAsset || !e.ExpiryTime.Equal(now.Add(90*time.Second)) || *e.EntityID != m.ID() {
			t.Errorf("%s: template %s, expiry %v, id %s", name, *e.Ontology.Template, e.ExpiryTime, *e.EntityID)
		}
	}
	if off := m.Offline(now); *off.Health.ConnectionStatus != entity.ConnectionOffline || *off.Health.HealthStatus != entity.HealthOffline {
		t.Error("the last publish should say offline")
	}
}

// TestID checks that the asset's ID is stable per feed and distinct across feeds.
func TestID(t *testing.T) {
	other := cfg
	other.Source = "tak-backup"
	mo, err := health.New(other)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := newMonitor(t).ID(), newMonitor(t).ID(); a != b || a == mo.ID() {
		t.Errorf("IDs %s %s %s: want the same feed to match and different feeds not to", a, b, mo.ID())
	}
}

// TestConfig checks the config's validation and defaults.
func TestConfig(t *testing.T) {
	for name, c := range map[string]health.Config{
		"no source":             {Latitude: 1, Longitude: 1, Interval: time.Second},
		"no interval":           {Source: "s", Latitude: 1, Longitude: 1},
		"interval over 30s":     {Source: "s", Latitude: 1, Longitude: 1, Interval: time.Minute},
		"latitude out of range": {Source: "s", Latitude: 91, Longitude: 1, Interval: time.Second},
	} {
		if _, err := health.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if e := newMonitor(t).Snapshot(now, health.Totals{}, up); *e.Aliases.Name != "takgw (tak-main)" {
		t.Errorf("default name %q", *e.Aliases.Name)
	}
}
