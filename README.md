# TAK Interop Gateway

A gateway that translates TAK Cursor-on-Target (CoT) traffic into entities for the Anduril Lattice SDK, with a
validator, Entity Preflight, that checks every entity against the platform's documented acceptance rules before it is
sent.

> **Status: early development.** The whole path works: `takgw serve` reads CoT from stdin, from TAK clients
> connecting in (plain TCP or mutual TLS), or from a TAK server it connects to over mutual TLS; maps it, filters out
> repeats, late arrivals, impossible jumps and impersonation, checks it with Entity Preflight, and publishes it, as
> JSON lines or to Lattice through the official Go SDK, alongside an asset for the gateway itself that shows whether
> its feed can be trusted; and `takgw readback` checks what the platform stored. The Lattice side is tested against an
> in-process fake of its REST API, and the TAK side against local TLS stand-ins; neither has yet been run against a
> live system.

## Components

One binary, `takgw`, with four commands, and two test tools:

- `takgw serve`: the gateway. Reads CoT from stdin, a listener (plain TCP or mutual TLS), or a TAK server's
  streaming TLS port, maps each event to an entity, drops repeats, late arrivals, unchanged reports, physically
  impossible jumps and other senders' claims on live tracks, checks what's left with Entity Preflight, and publishes
  it: as JSON lines by default, or to a Lattice environment with `-lattice`. With `-health-position` it also publishes
  itself as an asset.
- `takgw preflight`: Entity Preflight as a standalone tool. Reads entity JSON and reports any entity that breaks a
  documented rule, so other integrations can run it in their own CI.
- `takgw readback`: a round-trip check. Publishes the project's own simulated test entities, reads each back in a
  separate request, and reports any field the platform dropped or changed.
- `takgw devcerts`: writes a throwaway certificate authority, a server certificate and two client certificates for
  trying mutual TLS locally. Not for production.
- `cot-gen`: a synthetic, labeled search-and-rescue scenario as CoT, for test data (below).
- `hostile-sender`: fires a catalogue of hostile and broken CoT at a gateway you run, to show it refusing each.

## Running the gateway offline

`serve`'s output is `preflight`'s input, so the whole path can be checked with no Lattice environment:

```console
$ go run ./cmd/cot-gen -ticks 10 | go run ./cmd/takgw serve -stdin -source demo -simulated | go run ./cmd/takgw preflight -strict
takgw serve: 50 messages: 41 published, 9 dropped (unchanged 9), 0 unparsable, 0 unmappable, 0 rejected by preflight, 0 publish errors, 0 stream errors; 5 entities tracked
takgw preflight: 41 entities checked: 0 errors, 0 warnings, 0 unreadable
```

The walkers move every tick, so each of their reports is published. The last-known-position marker never moves, so
after its first publish it is held back until the 30-second refresh is due, on the gateway's clock. Over TCP, in real
time:

```bash
go run ./cmd/takgw serve -listen 127.0.0.1:8087 -source demo -simulated -out entities.jsonl   # Ctrl-C to stop
go run ./cmd/cot-gen -tcp 127.0.0.1:8087 -realtime -ticks 0                                   # in another terminal
```

`-listen` without TLS takes plain TCP with no authentication, so anyone who can reach the port can inject tracks:
bind it to a loopback address or a trusted network (serve warns when it isn't loopback), or use mutual TLS, below.
`-source` names the feed and is part of every entity ID, so keep it stable. Pass `-simulated` for test data. Run
`takgw serve -h` for every flag.

## Mutual TLS, and connecting to a TAK server

TAK servers authenticate clients by certificate, so the gateway speaks mutual TLS in both directions. With
`-tls-cert`, `-tls-key` and `-tls-ca`, `-listen` accepts only clients whose certificate the given authority signed,
and `-connect` dials a TAK server with the gateway's own certificate, trusting only the given authority for the
server's. There is no option to skip verification. To try it locally:

```bash
go run ./cmd/takgw devcerts -dir devcerts      # a throwaway CA, server and client certificate; never overwrites
go run ./cmd/takgw serve -source demo -simulated -listen 127.0.0.1:8089 \
  -tls-cert devcerts/server.pem -tls-key devcerts/server.key -tls-ca devcerts/ca.pem
go run ./cmd/cot-gen -tcp 127.0.0.1:8089 -realtime -ticks 0 \
  -tls-cert devcerts/client.pem -tls-key devcerts/client.key -tls-ca devcerts/ca.pem
```

serve logs each client's certificate name, and refuses a client without a trusted certificate at the handshake,
which must finish within 10 seconds. Against a TAK server's streaming port (often 8089), with a client certificate
the server's authority issued:

```bash
go run ./cmd/takgw serve -source tak-main -connect tak.example.org:8089 \
  -tls-cert takgw.pem -tls-key takgw.key -tls-ca tak-ca.pem
```

`-connect` reconnects with backoff (1 second doubling to a minute, with jitter) whenever the connection fails or the
server closes it, and only ever reads. TAK's certificate tools write PKCS #12 files; the gateway reads PEM, so convert
them with openssl (add `-legacy` if OpenSSL 3 refuses an older file):

```bash
openssl pkcs12 -in takgw.p12 -clcerts -nokeys -out takgw.pem          # the gateway's certificate
openssl pkcs12 -in takgw.p12 -nocerts -nodes -out takgw.key           # its private key: keep it private
openssl pkcs12 -in truststore.p12 -nokeys -out tak-ca.pem             # the server's authority
```

`.gitignore` excludes `devcerts/`, `*.key`, `*.p12` and `*.pfx`, so none of these end up in a commit.

## Feed integrity, and the gateway on the picture

A gateway is a trust boundary: whatever it accepts, the operating picture shows. Besides parsing strictly and
checking every entity with preflight, serve drops two kinds of report that parse fine but shouldn't be believed:

- **Impossible jumps.** A report whose position implies a speed nothing in its environment can reach, judged by the
  source's own timestamps less both reports' position uncertainty: a GPS glitch, a replayed track, a spoofed
  position. The limits are generous (about Mach 3.2 in the air, 500 km/h on land, 97 knots on the water, 49 knots
  below it, none in space), so only the physically impossible trips them.
- **Impersonation.** Each track is bound to the identity it is published from: the client certificate under mutual
  TLS, or the remote IP on plain TCP. While that identity keeps reporting, another one sending the same uid is
  dropped. The binding lapses after 90 seconds of silence, so a device that reconnects gets its track back and nobody
  can squat a uid.

Both are logged as warnings, and both show on the gateway's own asset. With `-health-position lat,lon[,hae]`, serve
publishes itself every 30 seconds as a friendly ground asset whose health rolls up three components: input (failing
while the TAK server connection is down), feed integrity (a warning while impossible jumps, impersonation, or a
spike of unparsable or rejected messages arrive), and publishing (failing while publishes fail). On a clean stop it
publishes itself once more as offline; if the gateway dies, the asset expires within 90 seconds. Its ID comes from a
namespace of its own, so no CoT uid can overwrite it.

`hostile-sender` fires 19 hostile cases at a gateway you run, each with the outcome the gateway must give; the same
catalogue is a test suite. Over mutual TLS, with the second identity `devcerts` writes for the impersonation case:

```bash
go run ./cmd/takgw devcerts -dir devcerts
go run ./cmd/takgw serve -source demo -simulated -listen 127.0.0.1:8089 -health-position 40.3428,-105.6836 \
  -tls-cert devcerts/server.pem -tls-key devcerts/server.key -tls-ca devcerts/ca.pem
go run ./cmd/hostile-sender -tcp 127.0.0.1:8089 -tls-ca devcerts/ca.pem \
  -tls-cert devcerts/client.pem -tls-key devcerts/client.key \
  -intruder-cert devcerts/intruder.pem -intruder-key devcerts/intruder.key
```

From one such run, with `cot-gen` streaming a normal feed alongside:

```text
level=WARN msg="dropped a suspect report" name=HOSTILE-TEST reason=implausible err="... the report implies a speed nothing in its environment can reach ..."
level=WARN msg="dropped a suspect report" name=HOSTILE-TEST reason=origin-conflict err="... the entity is being published from another origin ..."
level=WARN msg="connection closed on error" err="transport: malformed CoT stream: XML syntax error on line 1: invalid character entity &lol9;"
level=WARN msg="connection closed on error" err="transport: CoT message exceeds the size limit: limit 65536 bytes"
takgw serve: 91 messages: 62 published, 17 dropped (repeated 1, out-of-order 1, unchanged 13, implausible 1, origin-conflict 1), 10 unparsable, 2 unmappable, 0 rejected by preflight, 0 publish errors, 2 stream errors; 10 entities tracked
```

The gateway's asset went from healthy to a warning ("in the last 5s: 1 implausible jump, 1 report from another
origin for a live track, 3 unparsable messages"), back to healthy, and to offline at the stop. `hostile-sender -list`
describes every case.

## Publishing to Lattice, and readback

`takgw readback` publishes six simulated test entities, built by the real parser and mapper, then reads each back
and diffs it field by field. With no flags it runs against an in-process fake of the Lattice REST API, through the
real SDK adapter, with random tokens, so the whole path, both credentials included, is exercised without an
environment:

```console
$ go run ./cmd/takgw readback
takgw readback: offline, against an in-process fake of the Lattice REST API (-live for a real environment)
PASS  SIM-SAR-1              9ee568df-43fb-592b-8503-f5ce4d660ba3  (1 field(s) added by the platform; -v lists them)
...
takgw readback: 6 entities: 6 passed, 0 failed
```

A field the platform drops or changes fails the run; a field it adds, such as `createdTime`, is listed with `-v` but
allowed. For a real environment, use a development sandbox you own, with credentials in the environment, never in a
file in the repository:

```bash
export LATTICE_ENDPOINT=<environment_id>.env.sandboxes.developer.anduril.com   # a hostname, no https://
export ENVIRONMENT_TOKEN=...                                                   # the environment's bearer token
export SANDBOXES_TOKEN=...                                                     # Sandboxes only
go run ./cmd/takgw readback -live                                              # test entities expire after 2 minutes
cot-gen -ticks 0 -realtime | takgw serve -stdin -source demo -simulated -lattice
```

The adapter sends the environment token as the bearer token and the Sandboxes token in `anduril-sandbox-authorization`,
as Anduril's guide says; it never logs either, and refuses plain http to anything but loopback. With `-lattice`, an
entity the platform rejects is logged and counted, and the gateway carries on; refused credentials stop it.

## Entity Preflight

`takgw preflight` reads Lattice entities in the REST API's JSON shape, one object per line, and checks each against
the rules in [docs/rules.md](docs/rules.md). Each rule cites the section of Anduril's public integration guide, or the
line of the Go SDK, that it encodes, pinned to the commit it was read from. Enum values are checked by asking the
installed SDK's own parsers, and field names by walking the SDK's own types, so a value like `"FRIENDLY"` (instead of
`"DISPOSITION_FRIENDLY"`) or a key like `source_update_time` is caught before the server silently drops or rejects it.

```console
$ takgw preflight -now 2026-11-27T18:00:00Z entities.jsonl
entities.jsonl:1: warning known-fields provenance.source_update_time: is not a JSON name the SDK declares; the REST API spells this field "sourceUpdateTime"
entities.jsonl:1: error provenance-data-type provenance.dataType: is not set
entities.jsonl:1: error enum-values milView.disposition: "FRIENDLY" is not a value of this enum in lattice-sdk-go v5.5.0
entities.jsonl:1: warning source-update-time-set provenance.sourceUpdateTime: is not set
entities.jsonl:1: warning classification-set dataClassification.default.level: is not set
takgw preflight: 1 entities checked: 2 errors, 3 warnings, 0 unreadable
```

It exits 0 when every entity passes, 1 when any breaks an error rule (or a warning rule, with `-strict`), and 2 for a
usage error or a line that isn't one entity. One checker sees every line in order, so a republish whose source update
time didn't change, which the server discards, is caught too. Run `takgw preflight -h` for every flag. The rules are
also a Go package, [`preflight`](preflight), for integrations that want to check entities in their own tests.

Passing preflight is necessary, not sufficient: the documentation can lag the server, which is what `readback` is for.

## Design

- **Ports and adapters.** A pure core (parsing, mapping, state, validation) with the network at the edges, so the core
  can be fuzzed and tested without a live environment. The offline publisher runs the same code path a real deployment
  will.
- **Bounded by design.** Frame size, XML depth and element count, queued messages, concurrent connections, idle
  connections and tracked entities all have limits, so a hostile or broken feed can't exhaust memory.
- **Hostile input is a test suite.** Every case `hostile-sender` sends has the exact outcome the gateway must give,
  and a test runs each through the real framer and pipeline.
- **A narrow SDK boundary.** The official Lattice Go SDK is imported only by Entity Preflight, which asks it for enum
  values and field names, and by the publish adapter. A depguard lint rule enforces this. No SDK code, generated stubs
  or `.proto` files are committed to this repository.
- **What preflight checks is what gets sent.** The adapter converts entities to the SDK's type through JSON, with no
  hand-written field mapping, and a test proves the SDK's request body matches the model's JSON field for field.

See [docs/architecture.md](docs/architecture.md).

## Building

```bash
go build ./...
go test -race ./...
```

## Fuzzing

```bash
go test -run='^$' -fuzz=FuzzParse ./internal/cot           # runs until Ctrl-C; add -fuzztime=60s to bound it
go test -run='^$' -fuzz=FuzzFramer ./internal/transport
go test -run='^$' -fuzz=FuzzMap ./internal/mapping
go test -run='^$' -fuzz=FuzzCheckJSON ./preflight
go test -run='^$' -fuzz=FuzzPipeline ./internal/pipeline
```

- `FuzzParse` checks that the CoT parser never panics and that anything it accepts survives a marshal and re-parse
  unchanged.
- `FuzzFramer` checks that splitting a TCP stream into messages doesn't depend on how the bytes arrive, never invents
  or reorders data, respects the size limit, and agrees with the parser.
- `FuzzMap` checks that every entity the mapper returns holds its invariants and survives a JSON round trip.
- `FuzzCheckJSON` checks that Entity Preflight never panics on arbitrary input, refuses what it can't read with a
  typed error, and gives the same answer every time.
- `FuzzPipeline` is the end-to-end oracle. It feeds a fuzzed TCP stream through the framer, parser, mapper, state
  stage, Entity Preflight and an in-memory publisher, and checks what comes out, not just that nothing crashed: every
  entity published must pass preflight cleanly, both as a value and as JSON, and every publish-or-drop decision must
  match an independent reference model of the state policy. Planting nine bugs, four in the mapper and five in the
  state stage, confirmed that it catches each one.

Failing inputs are saved under each package's `testdata/fuzz/` and replayed by plain `go test` from then on.

## Test data: `cot-gen`

`cot-gen` writes a synthetic search-and-rescue scenario as CoT events, one XML event per line. A search team walks
north abreast while a fixed marker shows the missing person's last known position. Every event is labeled as
simulated in its remarks, and a given `-seed` and `-start` always produce the same output.

```bash
# Three ticks of four team members, to stdout
go run ./cmd/cot-gen -ticks 3

# Stream to a local TAK server's TCP input in real time, until Ctrl-C
go run ./cmd/cot-gen -tcp 127.0.0.1:8087 -realtime -ticks 0
```

Run `go run ./cmd/cot-gen -h` for every flag.

## Disclaimer

This is an independent, unofficial project. It is not affiliated with, endorsed by or sponsored by Anduril Industries
or the TAK Product Center. Product names are trademarks of their respective owners. Use of the Lattice SDK is subject
to its own license terms.

## License

Apache License 2.0. See [LICENSE](LICENSE).
