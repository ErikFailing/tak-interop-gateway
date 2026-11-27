// Package preflight implements Entity Preflight: it checks a candidate Lattice entity against the platform's
// documented acceptance rules before the entity is published.
//
// Each rule has a stable ID, a severity and links to the public documents it encodes, pinned to the commit they were
// read at; docs/rules.md lists them all. Enum values are checked by asking the installed Go SDK's own parsers, and
// field names by walking the SDK's own types, so neither list is copied and both stay in step with the SDK this module
// builds against.
//
// A Checker is stateful: it remembers each entity's last source update time, so checking a stream in publish order
// also catches an update the server would discard. Check takes the project's SDK-free entity model; CheckJSON takes
// one entity as REST-shaped JSON and adds the checks that need the raw document.
//
// This package and internal/publish/latticesdk are the only places allowed to import the SDK.
package preflight
