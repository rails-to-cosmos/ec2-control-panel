// Package tasks provides an in-memory async task queue used by the HTTP server
// to run long-lived operations (start/stop/restart) without blocking the
// submitter request. A per-session lock prevents concurrent destructive ops.
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Task is one async operation. It implements io.Writer so the progress.Logf
// machinery can stream lines straight into the task's output buffer.
type Task struct {
	// Immutable after creation.
	ID        string
	Operation string
	SessionID string
	CreatedAt time.Time

	mu         sync.Mutex
	status     Status
	output     []byte
	finishedAt *time.Time
	errMsg     string

	done chan struct{}
}

func (t *Task) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.output = append(t.output, b...)
	return len(b), nil
}

// Snapshot returns any output written past offset, current status, error, and
// whether the task is finished. Used by the polling stream endpoint.
func (t *Task) Snapshot(offset int) (data []byte, status Status, errMsg string, isFinal bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if offset < len(t.output) {
		data = make([]byte, len(t.output)-offset)
		copy(data, t.output[offset:])
	}
	status = t.status
	errMsg = t.errMsg
	isFinal = status == StatusCompleted || status == StatusFailed
	return
}

func (t *Task) Summary(includeOutput bool) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := map[string]any{
		"id":        t.ID,
		"operation": t.Operation,
		"sessionId": t.SessionID,
		"status":    string(t.status),
		"createdAt": t.CreatedAt.Format(time.RFC3339),
	}
	if t.finishedAt != nil {
		m["finishedAt"] = t.finishedAt.Format(time.RFC3339)
	}
	if t.errMsg != "" {
		m["error"] = t.errMsg
	}
	if includeOutput {
		m["output"] = string(t.output)
	}
	return m
}

func (t *Task) IsDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status == StatusCompleted || t.status == StatusFailed
}

func (t *Task) setStatus(s Status) {
	t.mu.Lock()
	t.status = s
	t.mu.Unlock()
}

// Manager owns the set of tasks. In-memory only (no persistence — tasks
// vanish on server restart, by design for v1).
type Manager struct {
	mu       sync.Mutex
	tasks    map[string]*Task
	order    []string          // task IDs, oldest-first; eviction + listing
	active   map[string]string // sessionID → currently-running taskID
	maxTasks int
}

func NewManager(maxTasks int) *Manager {
	return &Manager{
		tasks:    make(map[string]*Task),
		active:   make(map[string]string),
		maxTasks: maxTasks,
	}
}

// Create reserves a new task for sessionID. Refuses with an error if another
// task for the same session is still running (the per-session lock).
func (tm *Manager) Create(operation, sessionID string) (*Task, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if existingID, ok := tm.active[sessionID]; ok {
		if existing, ok2 := tm.tasks[existingID]; ok2 && !existing.IsDone() {
			return nil, fmt.Errorf("a %s task is already running for %q (taskId %s)",
				existing.Operation, sessionID, existing.ID)
		}
	}
	t := &Task{
		ID:        newTaskID(),
		Operation: operation,
		SessionID: sessionID,
		CreatedAt: time.Now(),
		status:    StatusPending,
		done:      make(chan struct{}),
	}
	tm.tasks[t.ID] = t
	tm.order = append(tm.order, t.ID)
	tm.active[sessionID] = t.ID
	tm.evict()
	return t, nil
}

func (tm *Manager) Get(id string) (*Task, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t, ok := tm.tasks[id]
	return t, ok
}

// List returns tasks newest-first.
func (tm *Manager) List() []*Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	out := make([]*Task, 0, len(tm.order))
	for i := len(tm.order) - 1; i >= 0; i-- {
		if t, ok := tm.tasks[tm.order[i]]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Run launches fn in a goroutine, marking the task running on entry and
// completed/failed on exit. fn should write progress through w (the task itself).
func (tm *Manager) Run(t *Task, fn func(ctx context.Context, w io.Writer) error) {
	go func() {
		t.setStatus(StatusRunning)
		// Detach from any HTTP request context — task outlives the submitter request.
		ctx := context.Background()
		err := fn(ctx, t)
		tm.finish(t, err)
	}()
}

func (tm *Manager) finish(t *Task, err error) {
	tm.mu.Lock()
	if cur, ok := tm.active[t.SessionID]; ok && cur == t.ID {
		delete(tm.active, t.SessionID)
	}
	tm.mu.Unlock()

	t.mu.Lock()
	now := time.Now()
	t.finishedAt = &now
	if err != nil {
		t.status = StatusFailed
		t.errMsg = err.Error()
	} else {
		t.status = StatusCompleted
	}
	t.mu.Unlock()
	close(t.done)
}

func (tm *Manager) evict() {
	for len(tm.order) > tm.maxTasks {
		id := tm.order[0]
		tm.order = tm.order[1:]
		// Don't evict an active task (rare with sensible maxTasks, but safe).
		if t, ok := tm.tasks[id]; ok && !t.IsDone() {
			tm.order = append([]string{id}, tm.order...)
			return
		}
		delete(tm.tasks, id)
	}
}

func newTaskID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
