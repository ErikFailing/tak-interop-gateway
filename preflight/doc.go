// Package preflight implements Entity Preflight: it checks a candidate entity against the documented acceptance rules
// of the Lattice platform before the entity is sent. Each rule has an ID, a reason and a link to the public
// documentation it encodes. The package is public so that other Go integrations can import it.
package preflight
