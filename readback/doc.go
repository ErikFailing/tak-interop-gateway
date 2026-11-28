// Package readback publishes entities, reads each one back in a separate request, and reports every field that came
// back different from what was sent. It catches what only the server knows: a field that was accepted and then
// dropped, or a value it normalized. Entity Preflight checks an entity against the documentation before it is sent;
// readback checks it against the platform afterward, which is the final word when the documentation lags.
//
// The package has no SDK dependency. It talks to the platform through the small Client interface, which the
// gateway's SDK adapter implements, and compares entities as JSON in the REST API's shape.
package readback
