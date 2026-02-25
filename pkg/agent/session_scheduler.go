package agent

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/logger"
)

type sessionScheduler struct {
	maxConcurrent int
	laneQueueSize int
	sem           chan struct{}
	mu            sync.Mutex
	lanes         map[string]*laneWorker
	stopped       atomic.Bool
	wg            sync.WaitGroup
}

type laneWorker struct {
	queue chan func()
}

const defaultLaneQueueSize = 128

func newSessionScheduler(maxConcurrent int) *sessionScheduler {
	return newSessionSchedulerWithQueue(maxConcurrent, defaultLaneQueueSize)
}

func newSessionSchedulerWithQueue(maxConcurrent, laneQueueSize int) *sessionScheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if laneQueueSize <= 0 {
		laneQueueSize = defaultLaneQueueSize
	}
	return &sessionScheduler{
		maxConcurrent: maxConcurrent,
		laneQueueSize: laneQueueSize,
		sem:           make(chan struct{}, maxConcurrent),
		lanes:         make(map[string]*laneWorker),
	}
}

func (s *sessionScheduler) Submit(laneKey string, fn func()) bool {
	if fn == nil {
		return false
	}
	if s.stopped.Load() {
		return false
	}
	s.mu.Lock()
	if s.stopped.Load() {
		s.mu.Unlock()
		return false
	}
	lane := s.getLaneLocked(laneKey)
	s.wg.Add(1)
	s.mu.Unlock()

	if s.stopped.Load() {
		s.wg.Done()
		return false
	}

	enqueued := true
	func() {
		defer func() {
			if recover() != nil {
				enqueued = false
			}
		}()
		lane.queue <- fn
	}()
	if !enqueued {
		s.wg.Done()
		return false
	}
	return true
}

func (s *sessionScheduler) Stop() {
	if s.stopped.Swap(true) {
		return
	}
	s.mu.Lock()
	for _, lane := range s.lanes {
		close(lane.queue)
	}
	s.mu.Unlock()
}

func (s *sessionScheduler) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *sessionScheduler) acquire() {
	s.sem <- struct{}{}
}

func (s *sessionScheduler) release() {
	select {
	case <-s.sem:
	default:
	}
}

func (s *sessionScheduler) getLaneLocked(laneKey string) *laneWorker {
	if laneKey == "" {
		laneKey = "default"
	}
	lane := s.lanes[laneKey]
	if lane == nil {
		lane = &laneWorker{queue: make(chan func(), s.laneQueueSize)}
		s.lanes[laneKey] = lane
		go s.runLane(lane)
	}
	return lane
}

func (s *sessionScheduler) runLane(lane *laneWorker) {
	for fn := range lane.queue {
		s.acquire()
		func() {
			defer s.release()
			defer s.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("agent", "Session lane task panicked", map[string]interface{}{
						"panic": fmt.Sprint(r),
					})
				}
			}()
			fn()
		}()
	}
}
