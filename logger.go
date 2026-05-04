package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

type ctxLoggerKey struct{}

// contextWithLogger returns a context whose attached writer logf() will use.
// The HTTP server uses this to direct progress lines to an SSE stream;
// the CLI uses the default (os.Stdout).
func contextWithLogger(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, ctxLoggerKey{}, w)
}

func loggerFromContext(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(ctxLoggerKey{}).(io.Writer); ok {
		return w
	}
	return os.Stdout
}

// logf writes a progress line to the context-bound writer (default os.Stdout).
// Use this everywhere the CLI used to call fmt.Printf so the same handler can
// drive both terminal output and the HTTP SSE stream.
func logf(ctx context.Context, format string, args ...interface{}) {
	fmt.Fprintf(loggerFromContext(ctx), format, args...)
}
