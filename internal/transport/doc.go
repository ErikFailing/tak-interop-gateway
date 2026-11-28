// Package transport moves CoT between the network and the gateway.
//
// Framer splits a stream of back-to-back CoT events, as a TAK server sends them over TCP, into one message per event.
// It finds boundaries with the same encoding/xml tokenizer cot.Parse uses, tracking element depth, so a "</event>" in
// a comment or CDATA section, or a nested <event> element, can't cut a message short, and the framer and parser can't
// disagree about where an element ends. Each frame is size-limited, and an error that loses the tokenizer's place ends
// the stream instead of guessing where the next message starts.
//
// ServerTLS and ClientTLS build the mutual-TLS configs for the two directions a gateway meets TAK: accepting clients
// that must present a certificate from a given authority, and connecting to a TAK server with this side's
// certificate while trusting only the server's authority. Neither can be told to skip verification.
package transport
