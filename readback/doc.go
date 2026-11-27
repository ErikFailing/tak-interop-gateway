// Package readback publishes the project's own test entities, reads them back in a separate step and reports any
// field that differs from what was sent. It catches what only the server knows: fields that were accepted and then
// dropped or normalized.
package readback
