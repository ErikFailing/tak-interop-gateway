// Package pipeline is the gateway's path for one CoT message: parse it, map it to a candidate entity, let the state
// stage drop repeats, late arrivals, unchanged reports, implausible jumps and other origins' claims, check what's left
// with Entity Preflight, and publish it.
// It has no network and no SDK of its own (the Publisher is passed in), so the whole path can be fuzzed with the
// in-memory publisher (FuzzPipeline) and then run unchanged by `takgw serve`.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/internal/publish"
	"github.com/erikfailing/tak-interop-gateway/internal/state"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// The stage at which Process stopped a message. Every error from Process wraps exactly one of these, followed by the
// stage's own cause (a cot, mapping, state or preflight sentinel, or the publisher's error), so a caller can count
// per stage and still use errors.Is on the specific reason. ErrDropped is routine traffic shaping, not a failure.
var (
	ErrParse    = errors.New("pipeline: CoT message refused by the parser")
	ErrMap      = errors.New("pipeline: event refused by the mapper")
	ErrDropped  = errors.New("pipeline: report dropped by the state stage")
	ErrRejected = errors.New("pipeline: entity rejected by Entity Preflight")
	ErrPublish  = errors.New("pipeline: publishing failed")
)

// Config assembles a pipeline for one feed.
type Config struct {
	Mapping   mapping.Config
	Limits    cot.Limits // the parser's limits; the zero value means cot.DefaultLimits
	State     state.Config
	Publisher publish.Publisher
	Clock     func() time.Time // read once per message; nil means time.Now
}

// Result is what the pipeline made of one message.
type Result struct {
	// Entity is the mapped entity, set once mapping succeeded, including when it was then dropped or rejected, so the
	// caller can log what it was.
	Entity entity.Entity
	// Violations are the entity's preflight findings: warnings and notes when it was published, at least one error
	// when Process returned ErrRejected.
	Violations []preflight.Violation
}

// Pipeline processes the messages of one CoT feed, in arrival order. It is not safe for concurrent use: its state
// stage remembers each entity's last publish, which only means something for one ordered stream.
type Pipeline struct {
	mapping mapping.Config
	limits  cot.Limits
	clock   func() time.Time
	state   *state.Tracker
	pub     publish.Publisher
}

// New returns a pipeline for one feed. It checks the config now, so a bad config fails at startup rather than on
// every message.
func New(cfg Config) (*Pipeline, error) {
	// 1: Check the parts and fill in the defaults.
	if err := cfg.Mapping.Validate(); err != nil {
		return nil, err
	}
	if cfg.Publisher == nil {
		return nil, errors.New("pipeline: no publisher")
	}
	tracker, err := state.New(cfg.State)
	if err != nil {
		return nil, err
	}
	p := &Pipeline{mapping: cfg.Mapping, limits: cfg.Limits, clock: cfg.Clock, state: tracker, pub: cfg.Publisher}
	if p.limits == (cot.Limits{}) {
		p.limits = cot.DefaultLimits
	}
	if p.clock == nil {
		p.clock = time.Now
	}
	return p, nil
}

// Tracked returns how many entities the state stage currently remembers.
func (p *Pipeline) Tracked() int {
	return p.state.Len()
}

// Process takes one framed CoT message all the way to the publisher. It returns nil once the entity has been
// published, and otherwise an error naming the stage that stopped it. origin says where the message came from, such
// as the client certificate of its connection: the state stage binds each entity to the origin it is published from,
// so another origin can't take over a live track.
func (p *Pipeline) Process(ctx context.Context, msg []byte, origin string) (Result, error) {
	// 1: One clock reading for the whole message. Every stage gets this same time: reading the clock again would let
	// a report that goes stale between mapping and checking pass one stage and fail the next.
	at := p.clock()

	// 2: Parse, then map.
	ev, err := p.limits.Parse(msg)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrParse, err)
	}
	e, err := mapping.Map(ev, p.mapping, at)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrMap, err)
	}
	res := Result{Entity: e}

	// 3: Let the state stage drop repeats, late arrivals, unchanged reports, implausible jumps and other origins'
	// claims before anything is checked or sent.
	if err := p.state.Admit(e, origin, at); err != nil {
		return res, fmt.Errorf("%w: %w", ErrDropped, err)
	}

	// 4: Check the entity. The state stage owns ordering and records only what was actually published, so preflight
	// runs here without history, on a fresh Checker: a Checker's own memory would count a report whose publish failed
	// as sent. An error-severity violation means the server would reject the entity, so it goes no further; warnings
	// and notes travel with the result.
	res.Violations = preflight.NewChecker(func() time.Time { return at }).Check(e)
	for _, v := range res.Violations {
		if v.Rule.Severity == preflight.Error {
			return res, fmt.Errorf("%w: %s", ErrRejected, v)
		}
	}

	// 5: Publish, and only then record it, so a failed publish doesn't make the next report look like a repeat.
	if err := p.pub.Publish(ctx, e); err != nil {
		return res, fmt.Errorf("%w: %w", ErrPublish, err)
	}
	p.state.Record(e, origin, at)
	return res, nil
}
