package server

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ec2cp/src/progress"
	"ec2cp/src/tasks"
)

// Pins that the task stream emits the task's complete output and terminates
// once the task finishes. Note it does NOT prove incremental delivery:
// httptest.ResponseRecorder discards flush timing, so a handler that buffered
// everything and wrote once at the end would pass this too.
func TestTaskStreamEmitsFullOutput(t *testing.T) {
	tm := tasks.NewManager(10)
	task, err := tm.Create("restart", "probe")
	if err != nil {
		t.Fatal(err)
	}
	tm.Run(task, func(ctx context.Context, w io.Writer) error {
		lctx := progress.WithLogger(ctx, w)
		for i := 1; i <= 3; i++ {
			progress.Logf(lctx, "step %d\n", i)
			time.Sleep(150 * time.Millisecond)
		}
		return nil
	})

	req := httptest.NewRequest("GET", "/api/tasks/"+task.ID+"/stream", nil)
	req.SetPathValue("taskID", task.ID)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { handleTaskStream(tm, nil)(rec, req); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not finish")
	}
	body := rec.Body.String()
	for i := 1; i <= 3; i++ {
		if !strings.Contains(body, fmt.Sprintf("step %d", i)) {
			t.Fatalf("missing step %d; body=%q", i, body)
		}
	}
	t.Logf("streamed body: %q", body)
}

// A task can sit silent for minutes waiting on AWS. Without a keepalive the
// proxy in front of the app drops the connection and the browser reports
// "TypeError: Error in input stream" mid-run.
func TestTaskStreamHeartbeatsWhileSilent(t *testing.T) {
	old := streamHeartbeat
	streamHeartbeat = 20 * time.Millisecond
	defer func() { streamHeartbeat = old }()

	tm := tasks.NewManager(10)
	task, err := tm.Create("start", "silent")
	if err != nil {
		t.Fatal(err)
	}
	tm.Run(task, func(ctx context.Context, w io.Writer) error {
		time.Sleep(300 * time.Millisecond) // produces no output at all
		return nil
	})

	req := httptest.NewRequest("GET", "/api/tasks/"+task.ID+"/stream", nil)
	req.SetPathValue("taskID", task.ID)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handleTaskStream(tm, nil)(rec, req); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not finish")
	}
	if n := strings.Count(rec.Body.String(), "\x00"); n == 0 {
		t.Fatalf("expected keepalive bytes during the silent run, got body %q", rec.Body.String())
	}
}
