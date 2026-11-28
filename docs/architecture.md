# Architecture

TAK Interop Gateway is built as ports and adapters: a pure core that does all the parsing, mapping, validation and
state tracking, with the network pushed out to adapters at the edges.

```
 TAK side (in)                     core (no network, no SDK)                                Lattice side (out)
┌───────────┐ bytes ┌────────────┐ event ┌─────────┐ entity ┌───────┐ new ┌───────────┐ pass ┌─────────────────┐
│ transport │ ────▶ │ framing +  │ ────▶ │ mapping │ ─────▶ │ state │ ──▶ │ preflight │ ───▶ │ publish adapter │
└───────────┘       │ CoT parser │       └─────────┘        └───────┘     └───────────┘      │ (JSONL, fake,   │
                    └────────────┘                              │                            │  later the SDK) │
                                                                ▼                            └─────────────────┘
                                                  drops repeats, late arrivals          records each entity in
                                                  and unchanged reports                 state only once it's sent
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
| `cmd/takgw` | The binary and its subcommands. `serve` reads CoT from stdin, a listener (plain TCP or mutual TLS, with a connection limit, a 10-second handshake limit, an idle timeout and a bounded queue in front of the pipeline) or a TAK server it connects to over mutual TLS, reconnecting with backoff; it publishes as JSON lines or to Lattice. `preflight` checks entity JSON lines. `readback` round-trips test entities. `devcerts` writes development certificates. |
| `cmd/cot-gen` | Writes a synthetic, labeled search-and-rescue scenario as CoT, to stdout or a TCP listener, plain or mutual TLS |
| `cmd/hostile-sender` | Fires the `internal/hostile` catalogue at a gateway you run: to stdout as one stream, or one TCP connection per case, with a second client identity for the impersonation case |
| `internal/cotgen` | The deterministic scenario behind `cot-gen` |
| `preflight` | Entity Preflight: the documented acceptance rules for a candidate entity, each with an ID, a severity and pinned links to the public documentation it encodes. See [rules.md](rules.md) and the section below. Public, so other Go projects can import it. |
| `readback` | Publishes entities, reads each back in a separate request, and diffs them field by field: dropped and changed fields fail, added ones (such as a server-filled `createdTime`) are reported. Timestamps naming the same instant, and numbers with the same value, count as equal however they're written. SDK-free: it talks through a two-method `Client` interface. Public. |
| `internal/transport` | `Framer`: splits a TCP stream into one size-limited message per event, using the parser's own tokenizer. `ServerTLS` and `ClientTLS`: mutual-TLS configs for accepting TAK clients and for connecting to a TAK server, TLS 1.2 or later, trusting only the authority given (not the system roots, since TAK servers run their own), with no way to skip verification. |
| `internal/devcert` | Writes a throwaway authority, a server certificate and two client certificates, the second an "intruder" for impersonation tests (P-256, 30 days, keys mode 0600, never overwriting), for `takgw devcerts` and for tests. Not for production. |
| `internal/hostile` | A catalogue of 19 hostile and broken inputs, each with the exact outcome the gateway must give: replays, rewinds, teleports, impersonation, a 2099 stale time, NaN and out-of-range coordinates, duplicate attributes and elements, deep nesting, a DTD, an undefined entity, a control character, a foreign namespace, impossible times, unknown affiliations, stray text and an oversized message. Its test runs every case through the real framer and pipeline. |
| `internal/cot` | The CoT event model (shared by the generator and the parser), and `Parse`: size, depth and element limits, one unnamespaced root, no directives or duplicates, then field validation |
| `entity` | An SDK-free model of the Lattice entity fields this project sets, with the REST API's JSON field names. Public, so Entity Preflight and other Go projects can use it. |
| `internal/mapping` | CoT event to candidate entity. See the mapping table below. |
| `internal/pipeline` | One message's whole path: parse, map, state, Entity Preflight as a gate, publish. Reads the clock once per message, so every stage agrees on "now", and wraps every refusal with its stage (`ErrParse`, `ErrMap`, `ErrDropped`, `ErrRejected`, `ErrPublish`) as well as the stage's own reason. `takgw serve` runs one pipeline per feed. |
| `internal/state` | The `Tracker`: drops a report that repeats or predates the last published one, implies an impossible speed, or comes from another origin than a live track's; holds back one that changes nothing until a refresh is due. Memory is bounded. See the section below. |
| `internal/publish` | The `Publisher` interface, with `Memory` (the in-process fake for tests) and `JSONL` (offline output, in the format `takgw preflight` reads). |
| `internal/publish/latticesdk` | The SDK adapter: publishes (`PUT /api/v1/entities`) and reads back (`GET /api/v1/entities/{id}`) through the official Go SDK. Credentials come from `LATTICE_ENDPOINT`, `ENVIRONMENT_TOKEN` and `SANDBOXES_TOKEN`; the config redacts them when printed, and errors never carry them. Plain http is refused except to loopback. Errors are classified as rejected (400), unauthorized (401) or not found (404), so callers can tell a bad entity from bad credentials. |
| `internal/latticefake` | An in-process stand-in for those two endpoints. It enforces both credentials, requires an `entityId`, stores what it's given and adds `createdTime`, the one server behavior the SDK documents. It does not validate entities: that's preflight's job, and only a live readback shows what the platform really does. |
| `internal/health` | Builds the gateway's own asset each interval: three health components (input, feed integrity, publishing) rolled up, one sensor standing for the feed, and an offline form for a clean stop. See the section below. |

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
- `FuzzPipeline` (in `internal/pipeline`) is the end-to-end oracle. It treats the input as a TCP stream from a TAK
  feed: the framer splits it, and one pipeline, publishing to the in-memory fake, processes every message. It checks
  what comes out, not just that nothing crashed:
  - every entity published has no preflight finding of any severity, and its JSON, checked from scratch, has none
    either;
  - preflight never rejects anything, because the mapper never builds an entity the server would refuse and the state
    stage removes repeats first;
  - each report the mapper accepts is published or dropped exactly as an independent reference model of the state
    policy predicts, with the same reason, so published updates strictly increase per entity and nothing that changed
    is ever held back;
  - every refusal wraps exactly one stage sentinel and a sentinel of that stage's own;
  - the fake publisher received exactly the entities reported as published, in order;
  - a second, fresh pipeline gives exactly the same outcomes for the same stream.

  The oracle was mutation-tested: nine planted bugs, four in the mapper (a wrong SIDC affiliation, a wrong
  environment, an empty display name, an expiry clamp of 31 days) and five in the state stage (the out-of-order check
  reversed, the repeat check removed, the entity limit not enforced, the expiry ignored when holding back an
  unchanged report, and the change digest including the expiry time), were each caught. The clamp was missed until a
  seed reached that branch, so the seeds now include a stale time decades out, as static markers send, and a report
  about to expire. CI runs it for 30 seconds on every push.
- `takgw serve` is tested end to end: a `cot-gen` scenario on stdin whose output then passes `takgw preflight
  -strict`, a live TCP connection stopped by cancelling its context, the connection limit, the idle timeout, every
  usage error, a malformed stream and a failing output. With `-lattice`, against the fake: entities published,
  refused credentials stopping the gateway, and rejected entities logged while it carries on.
- The SDK adapter is tested against the fake and against bare HTTP handlers. One test converts mapped entities, and
  one with every model field set, to the SDK's type and back to JSON, and requires the SDK's JSON to match the
  model's field for field: preflight checks the model's JSON, so this makes its verdict hold for the bytes actually
  sent. Others pin the request (method, path, both credential headers), the error classes, the base-URL rules, and
  that neither a printed config nor an error message ever contains a token.
- The hostile catalogue is a test: every case runs through the real framer and pipeline on its own stream and must get
  exactly its stated outcome. `serve` is also tested end to end with it: fed on stdin, it must turn the gateway's
  asset to a warning and then to offline at the stop, and over mutual TLS a second client certificate claiming a live
  track must be dropped. The teleport check is part of `FuzzPipeline`'s reference model, so every fuzzed report is
  published or dropped exactly as the speed limits say.
- The health monitor is tested for every roll-up (quiet, busy, a jump, a conflict, a spike, a lone bad message,
  failing publishes, input down), for counting only each window's change, and for every form of the asset passing
  preflight with no finding, as a value and as JSON.
- `takgw readback` is tested offline, and in live mode against fakes that store faithfully, drop a field, and refuse
  the credentials.
- Mutual TLS is tested as a handshake matrix over loopback, with two unrelated development authorities: a trusted
  client gets in and its certificate name is seen; a client with no certificate, one from the other authority, or
  one offering only TLS 1.1 is refused; a client refuses a server from the other authority or with the wrong name.
  Each bad file (missing, mismatched key, a CA file with no certificates) fails when the config is built, not at the
  first handshake. End to end, `serve -listen` with mutual TLS publishes a trusted client's reports and refuses a
  client without a certificate and one speaking plain TCP; `serve -connect` reconnects to a stand-in TAK server that
  closes after every tick, dropping the repeated reports; and it refuses a server signed by another authority, keeps
  retrying, and stops cleanly.

## Publishing to Lattice

`latticesdk.Client` converts each entity to the SDK's `Entity` by marshaling the model to JSON and unmarshaling it
into the SDK type, rather than copying fields by hand. The model's JSON names are the REST API's, and
`TestModelMatchesSDK` in `preflight` proves the SDK declares every one, so nothing can be silently left behind, and
there is no mapping code to fall out of date when the SDK changes.

The static environment token is passed with `option.WithToken`, which sets the `Authorization` header on every
request; set that way, it takes precedence over the SDK's OAuth client-credentials flow, which then never runs. The
Sandboxes token goes on the client itself, in `anduril-sandbox-authorization`, so it covers every request, as the
guide asks.

With `serve -lattice`, a publish failure is fatal only when the platform refuses the credentials, since every later
request would fail the same way. A rejected entity (HTTP 400) is logged and counted, and the gateway carries on. The
SDK itself makes up to two attempts, so one retry with backoff, on a 408, 429 or 5xx response.

## State

TAK networks rebroadcast, so the same report can arrive twice and an older one after a newer one. `state.Tracker`
sits between the mapper and preflight and decides, per entity, whether a report is worth publishing:

- **Ordering.** A report must be newer, by `provenance.sourceUpdateTime`, than the last one published for that
  entity. A repeat is dropped (the server would discard it anyway), and so is a late arrival, so a track never moves
  backward.
- **Change detection.** A newer report that changes nothing but its times is held back until the refresh is due
  (`-refresh`, 30 seconds by default), unless the published copy would expire within the refresh first. So a
  stationary unit is republished every 30 seconds, not every few seconds, and never lapses while it keeps reporting.
  The refresh runs on the gateway's clock.
- **Recorded only once sent.** The pipeline records an entity in the Tracker only after the publisher accepted it. A
  report that fails preflight, or whose publish fails, doesn't count as sent, so a rebroadcast of it can still go out.
  For the same reason preflight runs without history inside the pipeline: the Tracker owns ordering.
- **Bounded memory.** The Tracker forgets entities whose published expiry has passed, and refuses new ones beyond
  `-max-entities` (100,000 by default). A full Tracker scans for expired entities at most once a second, so a flood
  of new uids costs one scan a second, not one per message.

- **Impossible jumps.** A report whose move since the last publish implies a speed over its environment's limit is
  dropped. The speed is the great-circle distance, less both reports' uncertainty radius, over the time between their
  source timestamps, taken as at least a second. The limits (`state.DefaultMaxSpeed`) are generous bounds: 1,100 m/s
  in the air and for an unknown environment, 140 m/s on land, 50 m/s on the surface, 25 m/s below it, none in space.
  Because the clock is the source's, a device switched off and moved is fine: the time between its reports grows
  with the distance.
- **Origin binding.** Each entity is bound to the origin it was published from: the client certificate's name under
  mutual TLS, the remote IP on plain TCP (without the port, so a reconnect keeps it), `stdin`, or the TAK server's
  address for `-connect`, where every report shares one connection and so one origin. While that origin has
  published the entity within the last 90 seconds (three refreshes, so a stationary unit keeps it), another origin's
  report is dropped. A binding that only lasts while its owner keeps sending can't be squatted.

Two things the state stage doesn't do yet: notice the same CoT uid arriving from two different feeds, and tell a TAK
client that restarted (its clock may jump backward) from a late arrival.

## The gateway's own asset

With `-health-position`, serve publishes itself every `-health-interval` (30 seconds at most, as Anduril's asset
guide asks) as a `TEMPLATE_ASSET`: friendly, on land, at the given position. Its health rolls up three components,
each judged over the last interval: input (failing while the TAK server connection is down), feed integrity (a
warning on any impossible jump, origin conflict or refused new entity, or when unparsable and rejected messages are
at least 5 and at least a tenth of the interval's messages), and publishing (failing while publishes fail). Its one
sensor stands for the feed, as `SENSOR_TYPE_TRANSPONDER`, the closest type the SDK has to cooperative position
reports, and is operational, degraded or non-operational to match. It expires three intervals out, so a gateway that
dies drops off the picture on its own; a clean stop publishes it once more as offline, as the guide asks. Its ID is
a UUIDv5 under a namespace of its own, so no CoT uid can produce it. It goes through preflight like every entity,
and preflight's asset rules (`asset-health-set`, `asset-sensors-set`) and enum checks now cover health and sensors.
