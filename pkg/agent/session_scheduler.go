package agent

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dotsetgreg/dotagent/pkg/logger"
)

type sessionScheduler struct {
	maxConcurrent int
	laneQueueSize int
	laneIdleTTL   time.Duration
	laneSweep     time.Duration
	sem           chan struct{}
	mu            sync.Mutex
	lanes         map[string]*laneWorker
	stopped       atomic.Bool
	wg            sync.WaitGroup
	stopJanitor   chan struct{}
}

type laneWorker struct {
	key         string
	queue       chan func()
	lastTouched atomic.Int64
	running     atomic.Int32
	closed      atomic.Bool
}

const defaultLaneQueueSize = 128
const defaultLaneIdleTTL = 2 * time.Minute
const defaultLaneSweepInterval = 20 * time.Second

var (
	ErrSchedulerStopped    = errors.New("session scheduler stopped")
	ErrSchedulerNilTask    = errors.New("session scheduler received nil task")
	ErrSchedulerLaneClosed = errors.New("session scheduler lane closed")
	ErrSchedulerLaneFull   = errors.New("session scheduler lane queue full")
)

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
	s := &sessionScheduler{
		maxConcurrent: maxConcurrent,
		laneQueueSize: laneQueueSize,
		laneIdleTTL:   defaultLaneIdleTTL,
		laneSweep:     defaultLaneSweepInterval,
		sem:           make(chan struct{}, maxConcurrent),
		lanes:         make(map[string]*laneWorker),
		stopJanitor:   make(chan struct{}),
	}
	go s.runJanitor()
	return s
}

func (s *sessionScheduler) Submit(laneKey string, fn func()) error {
	if fn == nil {
		return ErrSchedulerNilTask
	}
	if s.stopped.Load() {
		return ErrSchedulerStopped
	}

	for attempts := 0; attempts < 2; attempts++ {
		s.mu.Lock()
		if s.stopped.Load() {
			s.mu.Unlock()
			return ErrSchedulerStopped
		}
		lane := s.getLaneLocked(laneKey)
		lane.lastTouched.Store(time.Now().UnixNano())
		s.wg.Add(1)
		s.mu.Unlock()

		if s.stopped.Load() {
			s.wg.Done()
			return ErrSchedulerStopped
		}

		if err := s.enqueue(lane, fn); err == nil {
			return nil
		} else if errors.Is(err, ErrSchedulerLaneClosed) {
			s.wg.Done()
			continue
		} else {
			s.wg.Done()
			return err
		}
	}

	return ErrSchedulerLaneClosed
}

func (s *sessionScheduler) enqueue(lane *laneWorker, fn func()) (err error) {
	if lane == nil {
		return ErrSchedulerLaneClosed
	}
	defer func() {
		if recover() != nil {
			err = ErrSchedulerLaneClosed
		}
	}()
	select {
	case lane.queue <- fn:
		return nil
	default:
		if lane.closed.Load() {
			return ErrSchedulerLaneClosed
		}
		if s.stopped.Load() {
			return ErrSchedulerStopped
		}
		return ErrSchedulerLaneFull
	}
}

func (s *sessionScheduler) Stop() {
	if s.stopped.Swap(true) {
		return
	}
	close(s.stopJanitor)
	s.mu.Lock()
	for _, lane := range s.lanes {
		lane.closed.Store(true)
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
	if lane == nil || lane.closed.Load() {
		if lane != nil && lane.closed.Load() {
			delete(s.lanes, laneKey)
		}
		lane = &laneWorker{
			key:   laneKey,
			queue: make(chan func(), s.laneQueueSize),
		}
		lane.lastTouched.Store(time.Now().UnixNano())
		s.lanes[laneKey] = lane
		go s.runLane(lane)
	}
	return lane
}

func (s *sessionScheduler) runLane(lane *laneWorker) {
	for fn := range lane.queue {
		lane.running.Add(1)
		lane.lastTouched.Store(time.Now().UnixNano())
		s.acquire()
		func() {
			defer s.release()
			defer s.wg.Done()
			defer lane.running.Add(-1)
			defer func() {
				if r := recover(); r != nil {
					logger.ErrorCF("agent", "Session lane task panicked", map[string]interface{}{
						"panic": fmt.Sprint(r),
					})
				}
			}()
			fn()
			lane.lastTouched.Store(time.Now().UnixNano())
		}()
	}
	lane.closed.Store(true)
	s.mu.Lock()
	if existing, ok := s.lanes[lane.key]; ok && existing == lane {
		delete(s.lanes, lane.key)
	}
	s.mu.Unlock()
}

func (s *sessionScheduler) runJanitor() {
	ticker := time.NewTicker(s.laneSweep)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.evictIdleLanes()
		case <-s.stopJanitor:
			return
		}
	}
}

func (s *sessionScheduler) evictIdleLanes() {
	if s.stopped.Load() {
		return
	}
	cutoff := time.Now().Add(-s.laneIdleTTL).UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, lane := range s.lanes {
		if lane == nil || lane.closed.Load() {
			delete(s.lanes, key)
			continue
		}
		if lane.running.Load() > 0 {
			continue
		}
		if len(lane.queue) > 0 {
			continue
		}
		if lane.lastTouched.Load() > cutoff {
			continue
		}
		lane.closed.Store(true)
		close(lane.queue)
		delete(s.lanes, key)
	}
}
