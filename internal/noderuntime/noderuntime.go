// Package noderuntime downloads and manages a pinned Node.js distribution under
// TANDEM_HOME when the operator opts into a Tandem-managed Node runtime (as
// opposed to using a Node already installed on the host).
package noderuntime

import (
	"context"
	"io"
)

// Ensure makes a managed Node distribution of the given version available under
// root, whose executable layout matches config.ManagedNodePaths(root). It
// downloads and extracts the official build when absent and is idempotent and
// safe to call before every use.
//
// NOTE: stub implementation — the real download/verify/extract logic is filled
// in by the managed-Node-runtime workstream.
func Ensure(ctx context.Context, root, version string, log io.Writer) error {
	return nil
}
