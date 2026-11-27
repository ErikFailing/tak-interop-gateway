# TAK Interop Gateway

A gateway that translates TAK Cursor-on-Target (CoT) traffic into entities for the Anduril Lattice SDK, with a
validator, Entity Preflight, that checks every entity against the platform's documented acceptance rules before it is
sent.

> **Status: early development.** This repository is a skeleton. Nothing here is functional yet.

## Planned components

One binary, `takgw`, with three commands:

- `takgw serve`: the gateway. Reads CoT from a TAK server or client, maps each event to an entity, validates it and
  publishes it.
- `takgw preflight`: Entity Preflight as a standalone tool. Reads entity JSON and reports any entity that breaks a
  documented rule, so other integrations can run it in their own CI.
- `takgw readback`: a round-trip check. Publishes the project's own test entities and confirms that what comes back
  matches what was sent.

## Design

- **Ports and adapters.** A pure core (parsing, mapping, validation, state) with the network at the edges, so the core
  can be fuzzed and tested without a live environment.
- **One SDK boundary.** The official Lattice Go SDK will be imported by a single adapter package. No SDK code,
  generated stubs or `.proto` files are committed to this repository.

See [docs/architecture.md](docs/architecture.md).

## Building

```bash
go build ./...
go test -race ./...
```

## Disclaimer

This is an independent, unofficial project. It is not affiliated with, endorsed by or sponsored by Anduril Industries
or the TAK Product Center. Product names are trademarks of their respective owners. Use of the Lattice SDK is subject
to its own license terms.

## License

Apache License 2.0. See [LICENSE](LICENSE).
