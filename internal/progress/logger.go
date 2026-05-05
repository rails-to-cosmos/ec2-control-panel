// Package progress wires a context-bound io.Writer used as the live progress
// sink across the CLI and HTTP server. The CLI binds os.Stdout; the HTTP
// server binds an auto-flushing response writer (or a Task buffer).
package progress

import (
	"context"
	"fmt"
	"io"
	"os"
)

type ctxKey struct{}

// WithLogger returns a context whose attached writer Logf will use.
func WithLogger(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, ctxKey{}, w)
}

func FromContext(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(ctxKey{}).(io.Writer); ok {
		return w
	}
	return os.Stdout
}

// Logf writes a progress line to the context-bound writer (default os.Stdout).
func Logf(ctx context.Context, format string, args ...any) {
	fmt.Fprintf(FromContext(ctx), format, args...)
}
