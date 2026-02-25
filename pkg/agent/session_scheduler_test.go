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

	s.Submit("lane-a", func() {
		mu.Lock()
		order = append(order, "start-1")
		mu.Unlock()
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		order = append(order, "end-1")
		mu.Unlock()
	})
	s.Submit("lane-a", func() {
		mu.Lock()
		order = append(order, "start-2")
		order = append(order, "end-2")
		mu.Unlock()
	})

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

	s.Submit("lane-a", func() {
		started <- "a"
		<-release
	})
	s.Submit("lane-b", func() {
		started <- "b"
		<-release
	})

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

	release := make(chan struct{})
	if !s.Submit("lane-a", func() { <-release }) {
		t.Fatalf("failed to enqueue first lane-a task")
	}
	if !s.Submit("lane-a", func() { <-release }) {
		t.Fatalf("failed to enqueue buffered lane-a task")
	}

	thirdDone := make(chan struct{})
	go func() {
		_ = s.Submit("lane-a", func() {})
		close(thirdDone)
	}()

	// Third lane-a submit should block while queue is full.
	select {
	case <-thirdDone:
		t.Fatalf("expected third lane-a submit to block while queue is full")
	case <-time.After(40 * time.Millisecond):
	}

	start := time.Now()
	if !s.Submit("lane-b", func() {}) {
		t.Fatalf("expected lane-b submit to succeed")
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("lane-b submit blocked too long (%s) while lane-a queue was full", elapsed)
	}

	close(release)
	select {
	case <-thirdDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("third lane-a submit did not unblock after release")
	}

	if !s.Wait(3 * time.Second) {
		t.Fatalf("scheduler timed out waiting for no-head-of-line test")
	}
}
