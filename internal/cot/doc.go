// Package cot defines the Cursor-on-Target event model and parses CoT events defensively.
//
// Parse (or Limits.Parse, for custom limits) accepts exactly one well-formed UTF-8 <event> document. It checks the
// message size first, then scans the tokens to enforce the structure (nesting depth and element-count limits, a single
// unnamespaced root, no DTDs or other directives, no duplicate attributes, no duplicate singleton elements, nothing
// after the root), and only then decodes into Event and validates the field values. Every failure wraps one of the
// package's Err values, so callers can branch on errors.Is.
//
// The same Event type is what cotgen writes, so generated data, parsed data and the fuzz target share one model.
package cot
