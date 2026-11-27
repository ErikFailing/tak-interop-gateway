# Architecture

TAK Interop Gateway is built as ports and adapters: a pure core that does all the parsing, mapping, validation and
state tracking, with the network pushed out to adapters at the edges.

```
 TAK side (in)                 core (no network, no SDK)                          Lattice side (out)
┌───────────┐  bytes  ┌──────────────┐ event ┌─────────┐ entity ┌───────────┐ verdict ┌─────────────────┐
│ transport │ ──────▶ │ framing +    │ ────▶ │ mapping │ ─────▶ │ preflight │ ──────▶ │ publish adapter │
└───────────┘         │ CoT parser   │       └─────────┘        └───────────┘         │ (SDK or fake)   │
                      └──────────────┘            ▲                                   └─────────────────┘
                                             ┌─────────┐
                                             │  state  │  monotonic update times, change detection,
                                             └─────────┘  cross-source ID collisions
```

Keeping the core free of network and SDK code means:

1. Fuzz targets feed bytes straight into the core and check its invariants, with no live environment.
2. An in-process fake publisher gives tests, and an offline demo, the same code path as a real deployment.
3. The SDK is imported in only two places, `preflight` (for its enum values and field names) and the publish adapter,
   which keeps its license boundary easy to check. A depguard rule in `.golangci.yml` fails the lint job if any other
   package imports it.

## Packages

| Package | Responsibility |
|---|---|
| `cmd/takgw` | The binary and its subcommands (`serve`, `preflight`, `readback`) |
| `cmd/cot-gen` | Writes a synthetic, labeled search-and-rescue scenario as CoT, to stdout or a TCP listener |
| `internal/cotgen` | The deterministic scenario behind `cot-gen` |
| `preflight` | Entity Preflight: the documented acceptance rules for a candidate entity, each with an ID, a severity and pinned links to the public documentation it encodes. See [rules.md](rules.md) and the section below. Public, so other Go projects can import it. |
| `readback` | Round-trip verification of the project's own test entities. Public. |
| `internal/transport` | `Framer`: splits a TCP stream into one size-limited message per event, using the parser's own tokenizer. Connections to TAK servers and clients (TCP, TLS with client certificates) are still to come. |
| `internal/cot` | The CoT event model (shared by the generator and the parser), and `Parse`: size, depth and element limits, one unnamespaced root, no directives or duplicates, then field validation |
| `entity` | An SDK-free model of the Lattice entity fields this project sets, with the REST API's JSON field names. Public, so Entity Preflight and other Go projects can use it. |
| `internal/mapping` | CoT event to candidate entity. See the mapping table below. |
| `internal/state` | Per-entity source update times, change detection, cross-source ID collision checks |
| `internal/publish` | The `Publisher` interface, with an SDK adapter and an in-process fake |
| `internal/health` | Publishes the gateway itself as an asset with a health component |

## Mapping

`mapping.Map` turns one parsed CoT event into an `entity.Entity`. It takes the current time as a parameter, so it is
deterministic, and it refuses rather than guesses when an event doesn't map cleanly.

| CoT | Entity field | Notes |
|---|---|---|
| feed name + `uid` | `entityId` | A UUIDv5 under this project's fixed namespace, so it is the same on every republish |
| affiliation letter in `type` | `milView.disposition` | `f` friendly, `a` assumed friendly, `n` neutral, `u`/`o`/`x` unknown, `p` pending, `s` suspicious, `h` hostile. The 2525 exercise identities joker (`j`) and faker (`k`) map to suspicious and hostile, with `indicators.exercise` set. Any other letter is refused. |
| battle dimension letter in `type` | `milView.environment` | `P` space, `A` air, `G`/`F` land, `S` surface, `U` sub-surface, `X`/`Z` unknown. Any other letter is refused. |
| the whole `type` | `symbology.milStd2525C.sidc` | Transliterated: `a-f-G-U-C` becomes `SFGPUC---------`. When the type doesn't transliterate cleanly, a generic symbol from the affiliation and battle dimension alone (`a-f` becomes `SFZP-----------`), because the integration guide asks for a SIDC on every entity. |
| `type` | `ontology.platformType` | `ontology.template` is always `TEMPLATE_TRACK` |
| `stale` | `expiryTime` | An already-stale event is refused. An expiry 30 days or more out is clamped to just under 30 days. |
| `point` lat, lon, hae | `location.position` | Altitude only when `hae` isn't the 9999999 "unknown" value |
| `point` `ce` | `locationUncertainty.positionErrorEllipse` | A circle of radius `ce`. Probability is left unset: producers don't agree on what confidence `ce` represents. |
| `detail/track` course, speed | `location.velocityEnu` | East and north only. Up is left unset rather than claimed as zero. |
| `detail/contact` callsign | `aliases.name` | Falls back to the `uid`, because the server requires a non-empty name |
| `uid`, `time`, feed name | `provenance` | `integrationName` `tak-interop-gateway`, `dataType` `cot`, `sourceId`, `sourceUpdateTime`, `sourceDescription` |
| (per feed) | `indicators.simulated`, `tracked.trackQualityWrapper` | Set from the feed's config. Track quality is a policy choice, since CoT has nothing to derive it from; `mapping.DefaultTrackQuality` is the guide's conservative 2. |
| (per feed) | `dataClassification.default.level` | Set from the feed's config, because a classification has to come from whoever operates the feed. Defaults to `CLASSIFICATION_LEVELS_UNCLASSIFIED`, the guide's fallback. Always set. |

Only atom events (types starting `a-`) become entities. Chat, drawings and other types are refused for now.

## Entity Preflight

`preflight.Checker` checks a candidate entity against the rules listed in [rules.md](rules.md). That page is generated
from the code, and a test fails if the committed copy is stale.

- **Severities.** An error is something the documentation says the server rejects or discards, or a required field.
  A warning is the guide's production-quality checklist, or a mistake it warns is accepted and then silently lost. An
  info note says what wasn't checked.
- **Pinned sources.** Every rule links to the guide section or SDK line it encodes, at a fixed commit or tag. A test
  keeps `preflight.SDKVersion` equal to the SDK version in `go.mod`, so an SDK upgrade fails the build until the rules
  and line links are rechecked.
- **Nothing copied.** Enum values are checked with the SDK's own `New...FromString` constructors, plus the two
  `INVALID` sentinels the guide says never to use. Field names are checked by walking the document against the SDK's
  `Entity` type with reflection. Neither list is copied into this repository.
- **Stateful.** A Checker remembers each entity's last `provenance.sourceUpdateTime`, so checking a stream in publish
  order catches an update whose time didn't change, which Entity Manager discards.
- **Unambiguous input only.** `CheckJSON` refuses a document with a duplicate key, including keys that differ only in
  case: encoding/json would fold them together and keep the last, while another reader might keep the first.
- **Where the docs disagree, follow the server.** The guide's field table lists only `integrationName` as required in
  `provenance`, but its example server error is `provenance.dataType must be set`, so both are errors.

## Testing

- Unit tests are table-driven, with a positive and a negative case for every Entity Preflight rule.
- Fuzz targets follow Go convention: `FuzzXxx` functions in `_test.go` files next to the code they test, with seed
  corpora under `testdata/fuzz/`. Seeds are generated by this project; no captured third-party data is committed.
- `FuzzParse` (in `internal/cot`, seeded from `cot-gen` output) checks that the parser never panics and that anything
  it accepts re-marshals and re-parses to identical bytes. CI runs it for 30 seconds on every push.
- The parser rejects duplicate attributes and duplicate singleton elements (a second `<point>`, `<contact>`,
  `<track>`...). Go keeps the last one, other parsers often keep the first, so two systems could read the same message
  differently.
- `FuzzFramer` (in `internal/transport`) checks four properties of the framer:
  - delivering the same bytes in different read sizes gives the same frames;
  - the frames, joined in order, are a prefix of the input;
  - no frame exceeds the limit;
  - any frame the parser accepts frames back to exactly itself.

  CI runs it for 30 seconds on every push.
- `FuzzMap` (in `internal/mapping`) feeds parser-accepted events to the mapper. It checks:
  - `Map` never panics;
  - every result has a well-formed UUIDv5, an expiry within bounds, a name, the event's own position, a well-formed
    SIDC and a data classification;
  - its JSON survives a round trip unchanged.

  CI runs it for 30 seconds on every push.
- The mapper's UUIDv5 code is checked against the RFC 9562 example and against IDs computed independently with
  Python's `uuid` module. A JSON golden test pins the exact wire shape.
- Entity Preflight's table test breaks one field at a time and checks that exactly the expected rules fire, and a
  meta-check fails if any rule has no case that breaks it. A reflection test fills every field of the `entity` model
  and requires that the SDK declares every JSON name it produces.
- An integration test runs `cot-gen` through the parser, the mapper and Entity Preflight for 20 ticks, with one
  Checker throughout, and requires zero violations of any severity. Another maps every CoT affiliation and battle
  dimension and requires the same.
- `FuzzCheckJSON` (in `preflight`) feeds arbitrary bytes to `CheckJSON`. It must never panic, must refuse with
  `ErrInvalidJSON` or return violations that name a real rule, and must give the same answer on a second run. CI runs
  it for 30 seconds on every push.
- The pipeline fuzz oracle will go beyond "does not crash": every entity the pipeline emits from fuzzed CoT must also
  pass Entity Preflight.
