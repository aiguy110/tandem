// Package daemon will contain the native Tandem daemon implementation.
package daemon

import "errors"

// ErrNotReady prevents the migration skeleton from being mistaken for the production
// daemon before protocol and persistence parity are complete.
var ErrNotReady = errors.New("the Go Tandem daemon is not production-ready; continue using the Node daemon")
