// Package health builds the gateway's own entity: an asset that sits on the operating picture beside the tracks the
// gateway publishes, and says whether its feed can be trusted right now. It is republished on a fixed interval of at
// most 30 seconds, and expires on its own if the gateway stops publishing it.
//
// Its health rolls up three components. Input is the feed itself: failing while a TAK server connection is down, and a
// warning until the first attempt to connect has finished.
// Integrity is the feed's content: a warning while hostile or broken input spikes, such as implausible jumps, other
// origins claiming live tracks, or unparsable messages. Output is publishing: failing while publishes fail. The
// asset's one sensor, the feed, mirrors them: operational, degraded or non-operational. On a clean stop, the last
// publish says offline.
package health

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
)

// MaxInterval is the longest the gateway may go between publishes of its own asset.
const MaxInterval = 30 * time.Second

// namespace is the UUIDv5 namespace for the gateway's own entity, separate from the one the mapper uses for CoT
// tracks, so no CoT uid can ever produce the gateway's ID and overwrite its health (82a5c7e0-5a35-4b1c-9f3e-7d2c4e6b8a91).
var namespace = [16]byte{0x82, 0xa5, 0xc7, 0xe0, 0x5a, 0x35, 0x4b, 0x1c, 0x9f, 0x3e, 0x7d, 0x2c, 0x4e, 0x6b, 0x8a, 0x91}

// The thresholds for the integrity warning, per window: any suspect drop or suspect connection end, or unparsable and
// rejected messages making up at least suspectShare of the window's messages, and at least
// suspectMin of them, so one bad message in a quiet minute doesn't raise a warning.
const (
	suspectShare = 0.1
	suspectMin   = 5
)

// Config describes the gateway's asset.
type Config struct {
	Source          string                     // the feed name; it decides the asset's ID, as it does every track's
	Name            string                     // aliases.name; empty means "takgw (Source)"
	Latitude        float64                    // where the gateway runs, in WGS84 degrees: an asset needs a position
	Longitude       float64                    //
	Altitude        *float64                   // height above the ellipsoid in meters, if known
	Interval        time.Duration              // how often the asset is republished; at most MaxInterval
	IntegrationName string                     // provenance.integrationName; empty means mapping.DefaultIntegrationName
	Simulated       bool                       // mark the asset simulated, as for the feed's tracks
	Classification  entity.ClassificationLevel // empty means CLASSIFICATION_LEVELS_UNCLASSIFIED
	Input           string                     // what the feed is, for the sensor description, such as "TAK server tak.example.org:8089"
}

// Totals are the gateway's running counts since it started. The monitor works out each window from the difference.
type Totals struct {
	Messages, Published, Unparsable, Rejected, PublishErrors int
	// The suspect drops: implausible jumps, reports from another origin for a live track, reports stamped too far
	// ahead of the gateway's clock, and new entities refused at the entity limit.
	Implausible, OriginConflicts, FutureTimes, EntityLimit int
	// SuspectStreams counts connections ended by input the framer couldn't follow (an oversized message, broken XML)
	// and failed TLS handshakes. Idle timeouts and dropped connections are not suspect, so they aren't counted.
	SuspectStreams int
}

// minus is the change from earlier to t.
func (t Totals) minus(earlier Totals) Totals {
	return Totals{
		Messages: t.Messages - earlier.Messages, Published: t.Published - earlier.Published,
		Unparsable: t.Unparsable - earlier.Unparsable, Rejected: t.Rejected - earlier.Rejected,
		PublishErrors: t.PublishErrors - earlier.PublishErrors, Implausible: t.Implausible - earlier.Implausible,
		OriginConflicts: t.OriginConflicts - earlier.OriginConflicts, FutureTimes: t.FutureTimes - earlier.FutureTimes,
		EntityLimit: t.EntityLimit - earlier.EntityLimit, SuspectStreams: t.SuspectStreams - earlier.SuspectStreams,
	}
}

// Input is the state of the feed right now.
type Input struct {
	Up bool // false while the gateway can't receive, such as a TAK server connection that is down
	// Starting is true until the feed's first attempt to receive has finished, such as the first connection to a TAK
	// server: not known to be up yet, but not failing either, so it is a warning. Up is ignored while it is set.
	Starting bool
	Detail   string // a short description, such as "connected to tak.example.org:8089" or "2 connections"
}

// Monitor builds the asset each interval. It is not safe for concurrent use.
type Monitor struct {
	cfg  Config
	id   string
	last Totals // the totals at the previous snapshot
}

// New checks the config and returns a monitor.
func New(cfg Config) (*Monitor, error) {
	switch {
	case cfg.Source == "":
		return nil, errors.New("health: no source")
	case cfg.Interval <= 0 || cfg.Interval > MaxInterval:
		return nil, fmt.Errorf("health: interval must be positive and at most %s, got %s", MaxInterval, cfg.Interval)
	case !within(cfg.Latitude, -90, 90) || !within(cfg.Longitude, -180, 180):
		return nil, fmt.Errorf("health: position %v,%v is not a WGS84 latitude and longitude", cfg.Latitude, cfg.Longitude)
	case cfg.Altitude != nil && (math.IsNaN(*cfg.Altitude) || math.IsInf(*cfg.Altitude, 0)):
		return nil, fmt.Errorf("health: altitude %v is not a finite number", *cfg.Altitude)
	}
	if cfg.Name == "" {
		cfg.Name = "takgw (" + cfg.Source + ")"
	}
	if cfg.IntegrationName == "" {
		cfg.IntegrationName = mapping.DefaultIntegrationName
	}
	if cfg.Classification == "" {
		cfg.Classification = entity.ClassificationUnclassified
	}
	return &Monitor{cfg: cfg, id: mapping.UUIDv5(namespace, cfg.Source)}, nil
}

// within reports whether v is in [lo, hi]; false for NaN.
func within(v, lo, hi float64) bool {
	return v >= lo && v <= hi
}

// ID returns the asset's entity ID.
func (m *Monitor) ID() string {
	return m.id
}

// Snapshot builds the asset for the window since the previous snapshot, given the totals now and the input's state.
func (m *Monitor) Snapshot(now time.Time, totals Totals, in Input) entity.Entity {
	// 1: The window is the change since the last snapshot.
	w := totals.minus(m.last)
	m.last = totals
	span := m.cfg.Interval.String()

	// 2: Judge each component.
	input := component("input", "Input", entity.HealthHealthy, in.Detail)
	switch {
	case in.Starting:
		input = component("input", "Input", entity.HealthWarn, in.Detail)
	case !in.Up:
		input = component("input", "Input", entity.HealthFail, in.Detail)
	}
	integrity := component("integrity", "Feed integrity", entity.HealthHealthy,
		fmt.Sprintf("no suspect input in the last %s", span))
	if suspect := suspectInput(w); suspect != "" {
		integrity = component("integrity", "Feed integrity", entity.HealthWarn,
			fmt.Sprintf("in the last %s: %s", span, suspect))
	}
	// Some failed publishes are a warning, such as an entity or two the platform rejected; failing is for a window in
	// which nothing got through at all.
	output := component("output", "Publishing", entity.HealthHealthy,
		fmt.Sprintf("%d published in the last %s", w.Published, span))
	if w.PublishErrors > 0 {
		level := entity.HealthWarn
		if w.Published == 0 {
			level = entity.HealthFail
		}
		output = component("output", "Publishing", level,
			fmt.Sprintf("%d of %d publishes failed in the last %s", w.PublishErrors, w.PublishErrors+w.Published, span))
	}

	// 3: Roll them up, and mirror the feed in the sensor's state.
	overall := worst(input, integrity, output)
	sensorState := entity.SensorOperational
	switch {
	case *input.Health == entity.HealthFail:
		sensorState = entity.SensorNonOperational
	case *input.Health != entity.HealthHealthy || *integrity.Health != entity.HealthHealthy:
		sensorState = entity.SensorDegraded
	}
	return m.asset(now, overall, entity.ConnectionOnline, sensorState, []entity.ComponentHealth{input, integrity, output})
}

// Offline builds the asset's last publish before a clean stop: offline, with every component offline too.
func (m *Monitor) Offline(now time.Time) entity.Entity {
	stopped := func(id, name string) entity.ComponentHealth {
		return component(id, name, entity.HealthOffline, "the gateway stopped")
	}
	return m.asset(now, entity.HealthOffline, entity.ConnectionOffline, entity.SensorNonOperational,
		[]entity.ComponentHealth{stopped("input", "Input"), stopped("integrity", "Feed integrity"), stopped("output", "Publishing")})
}

// suspectInput describes the window's suspect input, or returns "" when there was too little to warn about.
func suspectInput(w Totals) string {
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	bad := w.Unparsable + w.Rejected
	alarming := w.Implausible+w.OriginConflicts+w.FutureTimes+w.EntityLimit+w.SuspectStreams > 0
	spike := bad >= suspectMin && float64(bad) >= suspectShare*float64(w.Messages)
	if !alarming && !spike {
		return ""
	}
	add(w.Implausible, "implausible jump", "implausible jumps")
	add(w.OriginConflicts, "report from another origin for a live track", "reports from another origin for a live track")
	add(w.FutureTimes, "report stamped too far in the future", "reports stamped too far in the future")
	add(w.EntityLimit, "new entity refused at the entity limit", "new entities refused at the entity limit")
	add(w.SuspectStreams, "connection ended by unreadable input or a failed handshake",
		"connections ended by unreadable input or failed handshakes")
	add(w.Unparsable, "unparsable message", "unparsable messages")
	add(w.Rejected, "entity rejected by preflight", "entities rejected by preflight")
	return strings.Join(parts, ", ")
}

// rank orders health levels from best to worst.
var rank = map[entity.HealthStatus]int{entity.HealthHealthy: 0, entity.HealthWarn: 1, entity.HealthFail: 2, entity.HealthOffline: 3}

// worst is the worst of the components' levels: the roll-up.
func worst(cs ...entity.ComponentHealth) entity.HealthStatus {
	w := entity.HealthHealthy
	for _, c := range cs {
		if rank[*c.Health] > rank[w] {
			w = *c.Health
		}
	}
	return w
}

// component builds one component with one message at its own level.
func component(id, name string, status entity.HealthStatus, message string) entity.ComponentHealth {
	return entity.ComponentHealth{
		ID: new(id), Name: new(name), Health: new(status),
		Messages: []entity.ComponentMessage{{Status: new(status), Message: new(message)}},
	}
}

// asset builds the gateway's entity. Its expiry is three intervals out, so one missed publish doesn't make it
// vanish, but a gateway that died does.
func (m *Monitor) asset(now time.Time, overall entity.HealthStatus, conn entity.ConnectionStatus, sensorState entity.OperationalState, cs []entity.ComponentHealth) entity.Entity {
	now = now.UTC()
	e := entity.Entity{
		EntityID:   new(m.id),
		IsLive:     new(true),
		ExpiryTime: new(now.Add(3 * m.cfg.Interval)),
		Aliases:    &entity.Aliases{Name: new(m.cfg.Name)},
		MilView:    &entity.MilView{Disposition: new(entity.DispositionFriendly), Environment: new(entity.EnvironmentLand)},
		Ontology:   &entity.Ontology{Template: new(entity.TemplateAsset), PlatformType: new("TAK Interop Gateway")},
		Location: &entity.Location{Position: &entity.Position{
			LatitudeDegrees: new(m.cfg.Latitude), LongitudeDegrees: new(m.cfg.Longitude), AltitudeHaeMeters: m.cfg.Altitude,
		}},
		// A friendly ground equipment symbol, unspecified beyond that: the gateway is a piece of equipment on the ground.
		Symbology: &entity.Symbology{MilStd2525C: &entity.MilStd2525C{SIDC: new("SFGPE----------")}},
		Provenance: &entity.Provenance{
			IntegrationName: new(m.cfg.IntegrationName), DataType: new("takgw-health"), SourceID: new("gateway"),
			SourceUpdateTime: new(now), SourceDescription: new(m.cfg.Source),
		},
		DataClassification: &entity.Classification{Default: &entity.ClassificationInformation{Level: new(m.cfg.Classification)}},
		Health: &entity.Health{
			HealthStatus: new(overall), ConnectionStatus: new(conn), Components: cs, UpdateTime: new(now),
		},
		Sensors: &entity.Sensors{Sensors: []entity.Sensor{{
			SensorID: new("cot-feed"), SensorType: new(entity.SensorTypeTransponder), OperationalState: new(sensorState),
			SensorDescription: new("Cursor-on-Target position reports from " + m.cfg.Input),
		}}},
	}
	if m.cfg.Simulated {
		e.Indicators = &entity.Indicators{Simulated: new(true)}
	}
	return e
}
