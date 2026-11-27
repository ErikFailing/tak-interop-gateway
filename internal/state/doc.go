// Package state keeps per-entity state across events: strictly increasing source update times, change detection so
// unchanged entities are not republished, and detection of entity IDs that collide across sources.
package state
