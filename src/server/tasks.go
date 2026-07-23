package server

import (
	"fmt"
	"net/http"
	"time"

	"ec2cp/src/tasks"
)

// handleTaskStream tails a task's output buffer: sends current output, then
// any new bytes as they're written, and closes when the task finishes. The
// client connection is independent of the task — close the stream and the
// task keeps running.
func handleTaskStream(tm *tasks.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := tm.Get(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher.Flush()

		offset := 0
		for {
			data, status, errMsg, final := t.Snapshot(offset)
			if len(data) > 0 {
				_, _ = w.Write(data)
				flusher.Flush()
				offset += len(data)
			}
			if final {
				if status == tasks.StatusFailed && errMsg != "" {
					fmt.Fprintf(w, "\nERROR: %s\n", errMsg)
					flusher.Flush()
				}
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}
