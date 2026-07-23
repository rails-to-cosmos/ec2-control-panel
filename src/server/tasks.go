package server

import (
	"fmt"
	"net/http"
	"time"

	"ec2cp/src/config"
	"ec2cp/src/tasks"
)

// taskReadable reports whether the requester may see this task. A task is tied
// to an instance, so it inherits that instance's reader ACL — without this the
// task endpoints leak operation logs (and session ids) for instances the user
// cannot otherwise see.
func taskReadable(t *tasks.Task, auth *AuthConfig, r *http.Request) bool {
	if auth == nil {
		return true
	}
	insts, err := config.LoadInstances()
	if err != nil {
		return false
	}
	inst, ok := insts[t.SessionID]
	if !ok {
		return false
	}
	user, isAdmin := auth.reader(r)
	return inst.CanRead(user, isAdmin)
}

// taskFields is the JSON shape shared by the list and get endpoints.
func taskFields(t *tasks.Task) map[string]any {
	data, status, errMsg, final := t.Snapshot(0)
	return map[string]any{
		"id":        t.ID,
		"operation": t.Operation,
		"sessionId": t.SessionID,
		"status":    string(status),
		"error":     errMsg,
		"final":     final,
		"bytes":     len(data),
		"createdAt": t.CreatedAt.Format(time.RFC3339),
		"output":    string(data),
	}
}

// lookupTask resolves the {id} path value and enforces the instance ACL,
// writing the 404/403 itself. ok=false means the response is already sent.
func lookupTask(tm *tasks.Manager, auth *AuthConfig, w http.ResponseWriter, r *http.Request) (*tasks.Task, bool) {
	t, ok := tm.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	if !taskReadable(t, auth, r) {
		http.Error(w, errNotAuthorizedInst, http.StatusForbidden)
		return nil, false
	}
	return t, true
}

// handleTaskList returns recent tasks, filtered to the instances the requester
// may read.
func handleTaskList(tm *tasks.Manager, auth *AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		for _, t := range tm.List() {
			if !taskReadable(t, auth, r) {
				continue
			}
			f := taskFields(t)
			delete(f, "output") // the list stays small; fetch one task for its log
			out = append(out, f)
		}
		writeJSON(w, map[string]any{"tasks": out})
	}
}

// handleTaskGet returns a task's status and full output — the way to inspect a
// run without holding a stream open.
func handleTaskGet(tm *tasks.Manager, auth *AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := lookupTask(tm, auth, w, r)
		if !ok {
			return
		}
		writeJSON(w, taskFields(t))
	}
}

// handleTaskStream tails a task's output buffer: sends current output, then
// any new bytes as they're written, and closes when the task finishes. The
// client connection is independent of the task — close the stream and the
// task keeps running.
func handleTaskStream(tm *tasks.Manager, auth *AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := lookupTask(tm, auth, w, r)
		if !ok {
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
