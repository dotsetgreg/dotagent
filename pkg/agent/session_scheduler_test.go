package agent

import (
	"sync"
	"testing"
	"time"
)

func TestSessionScheduler_SerializesSameLane(t *testing.T) {
	s := newSessionScheduler(4)
	defer s.Stop()

	var mu sync.Mutex
	order := []string{}

	if err := s.Submit("lane-a", func() {
		mu.Lock()
		order = append(order, "start-1")
		mu.Unlock()
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		order = append(order, "end-1")
		mu.Unlock()
	}); err != nil {
		t.Fatalf("submit lane-a task 1: %v", err)
	}
	if err := s.Submit("lane-a", func() {
		mu.Lock()
		order = append(order, "start-2")
		order = append(order, "end-2")
		mu.Unlock()
	}); err != nil {
		t.Fatalf("submit lane-a task 2: %v", err)
	}

	if !s.Wait(2 * time.Second) {
		t.Fatalf("scheduler timed out waiting for same-lane tasks")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"start-1", "end-1", "start-2", "end-2"}
	if len(order) != len(want) {
		t.Fatalf("unexpected order length: got=%v want=%v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("unexpected order at index %d: got=%v want=%v", i, order, want)
		}
	}
}

func TestSessionScheduler_AllowsDifferentLanesConcurrent(t *testing.T) {
	s := newSessionScheduler(2)
	defer s.Stop()

	started := make(chan string, 2)
	release := make(chan struct{})

	if err := s.Submit("lane-a", func() {
		started <- "a"
		<-release
	}); err != nil {
		t.Fatalf("submit lane-a: %v", err)
	}
	if err := s.Submit("lane-b", func() {
		started <- "b"
		<-release
	}); err != nil {
		t.Fatalf("submit lane-b: %v", err)
	}

	first := <-started
	_ = first
	select {
	case <-started:
		// second lane started before first completed -> concurrent execution works.
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("expected second lane to start concurrently")
	}

	close(release)
	if !s.Wait(2 * time.Second) {
		t.Fatalf("scheduler timed out waiting for concurrent lanes")
	}
}

func TestSessionScheduler_NoHeadOfLineBlockingAcrossLanes(t *testing.T) {
	s := newSessionSchedulerWithQueue(1, 1)
	defer s.Stop()

	started := make(chan struct{})
	release := make(chan struct{})
	if err := s.Submit("lane-a", func() {
		close(started)
		<-release
	}); err != nil {
		t.Fatalf("failed to enqueue first lane-a task: %v", err)
	}
	select {
	case <-started:
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("first lane-a task did not start in time")
	}
	if err := s.Submit("lane-a", func() { <-release }); err != nil {
		t.Fatalf("failed to enqueue buffered lane-a task: %v", err)
	}

	thirdDone := make(chan struct{})
	thirdErr := make(chan error, 1)
	go func() {
		thirdErr <- s.Submit("lane-a", func() {})
		close(thirdDone)
	}()

	// Third lane-a submit should fail quickly while queue is full.
	select {
	case <-thirdDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("expected third lane-a submit to fail quickly while queue is full")
	}
	if err := <-thirdErr; err == nil {
		t.Fatalf("expected lane-a overflow error")
	} else if err != ErrSchedulerLaneFull {
		t.Fatalf("expected ErrSchedulerLaneFull, got %v", err)
	}

	start := time.Now()
	if err := s.Submit("lane-b", func() {}); err != nil {
		t.Fatalf("expected lane-b submit to succeed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("lane-b submit blocked too long (%s) while lane-a queue was full", elapsed)
	}

	close(release)

	if !s.Wait(3 * time.Second) {
		t.Fatalf("scheduler timed out waiting for no-head-of-line test")
	}
}

func TestSessionScheduler_SubmitRejectedAfterStop(t *testing.T) {
	s := newSessionScheduler(1)
	s.Stop()

	if err := s.Submit("lane-a", func() {}); err == nil {
		t.Fatalf("expected submit error after stop")
	}
}
