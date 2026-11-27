// Package pipeline is the gateway's path for one CoT message: parse it, map it to a candidate entity, and check the
// entity with Entity Preflight before anything is published. It has no network and no SDK, so the whole path can be
// fuzzed (FuzzPipeline) and then reused unchanged by `takgw serve`.
package pipeline

import (
	"errors"
	"fmt"
	"time"

	"github.com/erikfailing/tak-interop-gateway/entity"
	"github.com/erikfailing/tak-interop-gateway/internal/cot"
	"github.com/erikfailing/tak-interop-gateway/internal/mapping"
	"github.com/erikfailing/tak-interop-gateway/preflight"
)

// The stage at which Process dropped a message. Every error from Process wraps exactly one of these, followed by the
// stage's own cause (a cot, mapping or preflight sentinel), so a caller can count drops per stage and still use
// errors.Is on the specific reason.
var (
	ErrParse    = errors.New("pipeline: CoT message refused by the parser")
	ErrMap      = errors.New("pipeline: event refused by the mapper")
	ErrRejected = errors.New("pipeline: entity rejected by Entity Preflight")
)

// Result is what the pipeline made of one message.
type Result struct {
	Entity entity.Entity
	// Violations are the entity's preflight findings. On success they are warnings and notes only; when Process
	// returns ErrRejected they include at least one error, and the entity must not be published.
	Violations []preflight.Violation
}

// Pipeline processes the messages of one CoT feed, in arrival order. It is not safe for concurrent use: its preflight
// Checker remembers each entity's last update, which only means something for one ordered stream.
type Pipeline struct {
	cfg     mapping.Config
	limits  cot.Limits
	clock   func() time.Time
	at      time.Time // when the current message is being processed; the mapper and the checker both use it
	checker *preflight.Checker
}

// New returns a pipeline for one feed. It checks the config now, so a bad config fails at startup rather than on
// every message. clock is read once per message.
func New(cfg mapping.Config, limits cot.Limits, clock func() time.Time) (*Pipeline, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	p := &Pipeline{cfg: cfg, limits: limits, clock: clock}
	// The checker reads the time the pipeline already read for this message, not the clock itself. Reading the clock
	// twice would let a report that goes stale between mapping and checking pass one stage and fail the other.
	p.checker = preflight.NewChecker(func() time.Time { return p.at })
	return p, nil
}

// Process takes one framed CoT message to an entity that is ready to publish. When it returns an error, nothing should
// be published; ErrRejected still returns the entity and its violations, for logging.
func (p *Pipeline) Process(msg []byte) (Result, error) {
	// 1: One clock reading for the whole message.
	p.at = p.clock()

	// 2: Parse, then map. Each refusal is wrapped with its stage.
	ev, err := p.limits.Parse(msg)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrParse, err)
	}
	e, err := mapping.Map(ev, p.cfg, p.at)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrMap, err)
	}

	// 3: Check the entity. An error-severity violation means the server would reject or discard it, so it goes no
	// further; warnings and notes travel with the result.
	vs := p.checker.Check(e)
	for _, v := range vs {
		if v.Rule.Severity == preflight.Error {
			return Result{Entity: e, Violations: vs}, fmt.Errorf("%w: %s", ErrRejected, v)
		}
	}
	return Result{Entity: e, Violations: vs}, nil
}
