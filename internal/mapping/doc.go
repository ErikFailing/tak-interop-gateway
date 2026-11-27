// Package mapping converts a parsed CoT event into a candidate Lattice entity (an entity.Entity): a deterministic
// UUIDv5 entity ID, disposition and environment from the CoT type's affiliation and battle dimension, a MIL-STD-2525C
// SIDC transliterated from the type, location with velocity and uncertainty, a clamped expiry, and provenance.
//
// The mapping is pure: the current time is a parameter, and Map refuses rather than guesses when an event doesn't
// map cleanly.
package mapping
