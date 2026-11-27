// Package publish defines the Publisher interface the core uses to send entities. Its adapters are the only code that
// talks to the outside world: one wraps the official Lattice SDK (the only package that will import it), and an
// in-process fake serves tests and offline demos.
package publish
