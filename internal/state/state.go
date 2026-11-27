// Package state decides, per entity, whether a freshly mapped report should be published at all. TAK networks
// rebroadcast, so the same report can arrive twice and an older one can arrive after a newer one. The Tracker drops
// both, so a track never moves backward. It also holds back a report that changes nothing until a refresh is due, so
// stationary units don't flood the platform, while still republishing in time to keep each entity from expiring.
//
// The Tracker's memory is bounded: it forgets entities whose last published expiry has passed, and refuses new
// entities beyond a fixed count, so a feed that invents endless uids can't grow it without limit.
package state

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
)

// Why Admit held a report back. A dropped report is normal traffic, not a failure, so callers count these rather than
// log them as errors.
var (
	ErrRepeated   = errors.New("state: the report repeats the last published update time")
	ErrOutOfOrder = errors.New("state: the report is older than the last published update")
	ErrUnchanged  = errors.New("state: nothing changed since the last publish, and no refresh is due yet")
	ErrFull       = errors.New("state: already tracking the maximum number of entities")
	ErrConfig     = errors.New("state: invalid config")
)

const (
	// DefaultRefresh is how often an unchanged entity is republished. It is well inside the two-minute stale time
	// TAK clients commonly use, so a stationary unit stays live.
	DefaultRefresh = 30 * time.Second
	// DefaultMaxEntities bounds the Tracker's memory: about 100 bytes per entity.
	DefaultMaxEntities = 100_000
	// sweepEvery limits how often a full Tracker scans for expired entities, so a flood of new uids at the limit
	// costs one scan a second rather than one per message.
	sweepEvery = time.Second
)

// Config tunes a Tracker. The zero value means the defaults.
type Config struct {
	// Refresh is the longest an unchanged entity goes without a republish. Zero means DefaultRefresh. To publish
	// every report, set it to a negative value.
	Refresh time.Duration
	// MaxEntities is how many entities the Tracker remembers at once. Zero means DefaultMaxEntities.
	MaxEntities int
}

// record is what the Tracker remembers about one entity's last publish.
type record struct {
	updated   time.Time // provenance.sourceUpdateTime
	published time.Time // when the gateway published it, by the gateway's clock
	expiry    time.Time // its expiryTime: the server forgets the entity then, so the Tracker can too
	content   [32]byte  // a digest of everything but the time fields, for change detection
}

// Tracker remembers the last published state of each entity. It is not safe for concurrent use; one Tracker serves
// one ordered stream of reports.
type Tracker struct {
	refresh   time.Duration
	max       int
	entities  map[string]record
	lastSweep time.Time
}

// New returns an empty Tracker.
func New(cfg Config) (*Tracker, error) {
	if cfg.MaxEntities < 0 {
		return nil, fmt.Errorf("%w: max entities must not be negative, got %d", ErrConfig, cfg.MaxEntities)
	}
	t := &Tracker{refresh: cfg.Refresh, max: cfg.MaxEntities, entities: map[string]record{}}
	if t.refresh == 0 {
		t.refresh = DefaultRefresh
	}
	if t.max == 0 {
		t.max = DefaultMaxEntities
	}
	return t, nil
}

// Len returns how many entities the Tracker currently remembers.
func (t *Tracker) Len() int {
	return len(t.entities)
}

// Admit decides whether e should be published at now. It returns nil to publish, or one of the sentinel errors above
// to hold the report back. Admit changes nothing; call Record once the entity has actually been published, so a
// report that then fails preflight or publishing doesn't count as sent. An entity without an ID or a source update
// time is admitted, so preflight can reject it with the reason.
func (t *Tracker) Admit(e entity.Entity, now time.Time) error {
	// 1: An entity seen for the first time is admitted if there's room.
	id, updated, ok := key(e)
	if !ok {
		return nil
	}
	r, seen := t.entities[id]
	if !seen {
		if len(t.entities) >= t.max {
			t.sweep(now)
		}
		if len(t.entities) >= t.max {
			return ErrFull
		}
		return nil
	}

	// 2: A report must be newer than the last one published, or it is a rebroadcast or a late arrival.
	switch {
	case updated.Equal(r.updated):
		return ErrRepeated
	case updated.Before(r.updated):
		return ErrOutOfOrder
	}

	// 3: A newer report that changes nothing waits for the refresh, unless the published copy would expire first.
	if d, ok := digest(e); ok && t.refresh > 0 && d == r.content && now.Sub(r.published) < t.refresh && r.expiry.Sub(now) > t.refresh {
		return ErrUnchanged
	}
	return nil
}

// Record notes that e was published at now.
func (t *Tracker) Record(e entity.Entity, now time.Time) {
	id, updated, ok := key(e)
	if !ok {
		return
	}
	r := record{updated: updated, published: now}
	r.content, _ = digest(e) // without a digest, the zero value matches no later report, so the next one publishes
	if e.ExpiryTime != nil {
		r.expiry = *e.ExpiryTime
	}
	t.entities[id] = r
}

// sweep forgets every entity whose last published expiry has passed, at most once per sweepEvery.
func (t *Tracker) sweep(now time.Time) {
	if now.Sub(t.lastSweep) < sweepEvery {
		return
	}
	t.lastSweep = now
	for id, r := range t.entities {
		if !r.expiry.After(now) {
			delete(t.entities, id)
		}
	}
}

// key returns the entity's ID and source update time, the two fields the Tracker orders reports by.
func key(e entity.Entity) (id string, updated time.Time, ok bool) {
	if e.EntityID == nil || e.Provenance == nil || e.Provenance.SourceUpdateTime == nil {
		return "", time.Time{}, false
	}
	return *e.EntityID, *e.Provenance.SourceUpdateTime, true
}

// digest summarizes everything about e except the two fields that change on every report, expiryTime and
// provenance.sourceUpdateTime, so two reports with equal digests differ only in time. It returns false if e can't be
// summarized, and then Admit treats the report as changed: publishing is the safe side of that mistake.
func digest(e entity.Entity) ([32]byte, bool) {
	// 1: Clear the time fields on a copy. The copy is shallow, so provenance is copied before its field is cleared.
	e.ExpiryTime = nil
	if e.Provenance != nil {
		p := *e.Provenance
		p.SourceUpdateTime = nil
		e.Provenance = &p
	}

	// 2: Hash the JSON form, which is canonical for this struct: fixed field order, no maps.
	// Only a NaN or an infinity can make an entity fail to marshal, and the mapper never produces one.
	b, err := json.Marshal(e)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(b), true
}
