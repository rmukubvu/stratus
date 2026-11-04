// Package control implements the Stratus control plane: a small, versioned
// API that lets CI pipelines and coding agents manage the emulator itself
// rather than the emulated AWS resources.
//
// It answers three questions:
//
//   - Is the emulator healthy and ready to take traffic?
//   - How do I get back to a clean slate (everything, or one service)?
//   - How do I capture the current state and return to it later?
//
// The package is organised as a small hexagon: the domain types (Catalog,
// Snapshot, Error) and the application Service sit in the middle and depend
// only on the ports in ports.go. Storage, Docker and HTTP are adapters.
package control
