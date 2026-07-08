package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"ec2cp/src/config"
	"ec2cp/src/progress"
)

// progressWriter flushes on every write so streamed lines reach the client immediately.
type progressWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.flusher.Flush()
	return n, err
}

// withStream wraps an op into an HTTP handler that streams progress.Logf
// output back to the client as plain chunked text. Op-specific arg parsing
// lives in each op.
func withStream(env *config.EnvConfig, op func(ctx context.Context, env *config.EnvConfig, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported by this transport", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied
		flusher.Flush()

		pw := &progressWriter{w: w, flusher: flusher}
		ctx := progress.WithLogger(r.Context(), pw)
		if err := op(ctx, env, r); err != nil {
			fmt.Fprintf(pw, "\nERROR: %v\n", err)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
