// Package progress carries optional, human-readable progress reports for
// long-running operations (e.g. spawning an agent) through a context, so deep
// callees can describe what they are waiting on without new parameters.
package progress

import "context"

type key struct{}

// Reporter receives a short human-readable description of the current phase.
type Reporter func(phase string)

// With returns a context whose Report calls are delivered to fn.
func With(ctx context.Context, fn Reporter) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, fn)
}

// Report announces the current phase to the context's reporter, if any.
func Report(ctx context.Context, phase string) {
	if fn, ok := ctx.Value(key{}).(Reporter); ok {
		fn(phase)
	}
}
