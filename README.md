# TAK Interop Gateway

A gateway that translates TAK Cursor-on-Target (CoT) traffic into entities for the Anduril Lattice SDK, with a
validator, Entity Preflight, that checks every entity against the platform's documented acceptance rules before it is
sent.

> **Status: early development.** Working today: the CoT parser and stream framer, the CoT-to-entity mapper, the
> offline pipeline that ties them to Entity Preflight, `takgw preflight`, and the `cot-gen` test-data generator.
> `takgw serve` and `takgw readback` are not implemented yet.

## Components

One binary, `takgw`, with three commands:

- `takgw serve` (planned): the gateway. Reads CoT from a TAK server or client, maps each event to an entity, validates
  it and publishes it.
- `takgw preflight`: Entity Preflight as a standalone tool. Reads entity JSON and reports any entity that breaks a
  documented rule, so other integrations can run it in their own CI.
- `takgw readback` (planned): a round-trip check. Publishes the project's own test entities and confirms that what
  comes back matches what was sent.

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

- **Ports and adapters.** A pure core (parsing, mapping, validation, state) with the network at the edges, so the core
  can be fuzzed and tested without a live environment.
- **A narrow SDK boundary.** The official Lattice Go SDK is imported only by Entity Preflight, which asks it for enum
  values and field names, and by the publish adapter. A depguard lint rule enforces this. No SDK code, generated stubs
  or `.proto` files are committed to this repository.

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
- `FuzzPipeline` is the end-to-end oracle. It feeds a fuzzed TCP stream through the framer, parser, mapper and Entity
  Preflight, and checks what comes out, not just that nothing crashed: every entity the gateway would publish must pass
  preflight cleanly, both as a value and as JSON. Planting bugs in the mapper (a wrong SIDC letter, a wrong
  environment, an empty name, an expiry clamp past 30 days) confirmed that it catches each one.

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
