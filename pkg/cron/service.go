package cron

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/adhocore/gronx"
)

type CronSchedule struct {
	Kind    string `json:"kind"`
	AtMS    *int64 `json:"atMs,omitempty"`
	EveryMS *int64 `json:"everyMs,omitempty"`
	Expr    string `json:"expr,omitempty"`
	TZ      string `json:"tz,omitempty"`
}

type CronPayload struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Command string `json:"command,omitempty"`
	Deliver bool   `json:"deliver"`
	Channel string `json:"channel,omitempty"`
	To      string `json:"to,omitempty"`
}

type CronJobState struct {
	NextRunAtMS *int64 `json:"nextRunAtMs,omitempty"`
	LastRunAtMS *int64 `json:"lastRunAtMs,omitempty"`
	LastStatus  string `json:"lastStatus,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

type CronJob struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	Enabled        bool         `json:"enabled"`
	Schedule       CronSchedule `json:"schedule"`
	Payload        CronPayload  `json:"payload"`
	State          CronJobState `json:"state"`
	CreatedAtMS    int64        `json:"createdAtMs"`
	UpdatedAtMS    int64        `json:"updatedAtMs"`
	DeleteAfterRun bool         `json:"deleteAfterRun"`
}

type CronStore struct {
	Version int       `json:"version"`
	Jobs    []CronJob `json:"jobs"`
}

type JobHandler func(job *CronJob) (string, error)

type CronService struct {
	storePath string
	store     *CronStore
	onJob     JobHandler
	mu        sync.RWMutex
	running   bool
	stopChan  chan struct{}
	gronx     *gronx.Gronx
}

const maxEveryIntervalMS = int64(365 * 24 * 60 * 60 * 1000)

func NewCronService(storePath string, onJob JobHandler) (*CronService, error) {
	cs := &CronService{
		storePath: storePath,
		onJob:     onJob,
		gronx:     gronx.New(),
	}
	// Initialize and load store on creation
	if err := cs.loadStore(); err != nil {
		return nil, err
	}
	return cs, nil
}

func normalizeSchedule(schedule CronSchedule) CronSchedule {
	schedule.Kind = strings.ToLower(strings.TrimSpace(schedule.Kind))
	schedule.Expr = strings.TrimSpace(schedule.Expr)
	schedule.TZ = strings.TrimSpace(schedule.TZ)
	return schedule
}

func (cs *CronService) validateSchedule(schedule CronSchedule, nowMS int64, requireFutureAt bool) error {
	switch schedule.Kind {
	case "at":
		if schedule.AtMS == nil {
			return fmt.Errorf("at schedule requires atMs")
		}
		if requireFutureAt && *schedule.AtMS <= nowMS {
			return fmt.Errorf("at schedule must be in the future")
		}
	case "every":
		if schedule.EveryMS == nil {
			return fmt.Errorf("every schedule requires everyMs")
		}
		if *schedule.EveryMS <= 0 {
			return fmt.Errorf("everyMs must be greater than zero")
		}
		if *schedule.EveryMS > maxEveryIntervalMS {
			return fmt.Errorf("everyMs must be <= %d", maxEveryIntervalMS)
		}
	case "cron":
		if schedule.Expr == "" {
			return fmt.Errorf("cron schedule requires expr")
		}
		if !gronx.IsValid(schedule.Expr) {
			return fmt.Errorf("invalid cron expression: %q", schedule.Expr)
		}
		if schedule.TZ != "" {
			if _, err := time.LoadLocation(schedule.TZ); err != nil {
				return fmt.Errorf("invalid timezone %q: %w", schedule.TZ, err)
			}
		}
	default:
		return fmt.Errorf("unsupported schedule kind %q", schedule.Kind)
	}
	return nil
}

func (cs *CronService) Start() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.running {
		return nil
	}

	if err := cs.loadStore(); err != nil {
		return fmt.Errorf("failed to load store: %w", err)
	}

	cs.recomputeNextRuns()
	if err := cs.saveStoreUnsafe(); err != nil {
		return fmt.Errorf("failed to save store: %w", err)
	}

	cs.stopChan = make(chan struct{})
	cs.running = true
	go cs.runLoop(cs.stopChan)

	return nil
}

func (cs *CronService) Stop() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if !cs.running {
		return
	}

	cs.running = false
	if cs.stopChan != nil {
		close(cs.stopChan)
		cs.stopChan = nil
	}
}

func (cs *CronService) runLoop(stopChan chan struct{}) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			cs.checkJobs()
		}
	}
}

func (cs *CronService) checkJobs() {
	cs.mu.Lock()

	if !cs.running {
		cs.mu.Unlock()
		return
	}

	now := time.Now().UnixMilli()
	var dueJobIDs []string

	// Collect jobs that are due (we need to copy them to execute outside lock)
	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if job.Enabled && job.State.NextRunAtMS != nil && *job.State.NextRunAtMS <= now {
			dueJobIDs = append(dueJobIDs, job.ID)
		}
	}

	// Reset next run for due jobs before unlocking to avoid duplicate execution.
	dueMap := make(map[string]bool, len(dueJobIDs))
	for _, jobID := range dueJobIDs {
		dueMap[jobID] = true
	}
	for i := range cs.store.Jobs {
		if dueMap[cs.store.Jobs[i].ID] {
			cs.store.Jobs[i].State.NextRunAtMS = nil
		}
	}

	if err := cs.saveStoreUnsafe(); err != nil {
		log.Printf("[cron] failed to save store: %v", err)
	}

	cs.mu.Unlock()

	// Execute jobs outside lock.
	for _, jobID := range dueJobIDs {
		cs.executeJobByID(jobID)
	}
}

func (cs *CronService) executeJobByID(jobID string) {
	startTime := time.Now().UnixMilli()

	cs.mu.RLock()
	var callbackJob *CronJob
	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if job.ID == jobID {
			jobCopy := *job
			callbackJob = &jobCopy
			break
		}
	}
	cs.mu.RUnlock()

	if callbackJob == nil {
		return
	}

	var err error
	if cs.onJob != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("cron job panic: %v\n%s", r, string(debug.Stack()))
				}
			}()
			_, err = cs.onJob(callbackJob)
		}()
	}

	// Now acquire lock to update state
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var job *CronJob
	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID == jobID {
			job = &cs.store.Jobs[i]
			break
		}
	}
	if job == nil {
		log.Printf("[cron] job %s disappeared before state update", jobID)
		return
	}

	job.State.LastRunAtMS = &startTime
	job.UpdatedAtMS = time.Now().UnixMilli()

	if err != nil {
		job.State.LastStatus = "error"
		job.State.LastError = err.Error()
	} else {
		job.State.LastStatus = "ok"
		job.State.LastError = ""
	}

	// Compute next run time
	if job.Schedule.Kind == "at" {
		if job.DeleteAfterRun && err == nil {
			cs.removeJobUnsafe(job.ID)
		} else {
			job.Enabled = false
			job.State.NextRunAtMS = nil
		}
	} else {
		nextRun := cs.computeNextRun(&job.Schedule, time.Now().UnixMilli())
		job.State.NextRunAtMS = nextRun
	}

	if err := cs.saveStoreUnsafe(); err != nil {
		log.Printf("[cron] failed to save store: %v", err)
	}
}

func (cs *CronService) computeNextRun(schedule *CronSchedule, nowMS int64) *int64 {
	if schedule.Kind == "at" {
		if schedule.AtMS != nil && *schedule.AtMS > nowMS {
			return schedule.AtMS
		}
		return nil
	}

	if schedule.Kind == "every" {
		if schedule.EveryMS == nil || *schedule.EveryMS <= 0 {
			return nil
		}
		next := nowMS + *schedule.EveryMS
		return &next
	}

	if schedule.Kind == "cron" {
		if schedule.Expr == "" {
			return nil
		}

		// Use gronx to calculate next run time
		now := time.UnixMilli(nowMS)
		if schedule.TZ != "" {
			if loc, err := time.LoadLocation(schedule.TZ); err == nil {
				now = now.In(loc)
			} else {
				log.Printf("[cron] invalid timezone %q for expr '%s': %v", schedule.TZ, schedule.Expr, err)
				return nil
			}
		}
		nextTime, err := gronx.NextTickAfter(schedule.Expr, now, false)
		if err != nil {
			log.Printf("[cron] failed to compute next run for expr '%s': %v", schedule.Expr, err)
			return nil
		}

		nextMS := nextTime.UnixMilli()
		return &nextMS
	}

	return nil
}

func (cs *CronService) recomputeNextRuns() {
	now := time.Now().UnixMilli()
	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if job.Enabled {
			job.State.NextRunAtMS = cs.computeNextRun(&job.Schedule, now)
		}
	}
}

func (cs *CronService) getNextWakeMS() *int64 {
	var nextWake int64
	hasWake := false
	for _, job := range cs.store.Jobs {
		if job.Enabled && job.State.NextRunAtMS != nil {
			if !hasWake || *job.State.NextRunAtMS < nextWake {
				nextWake = *job.State.NextRunAtMS
				hasWake = true
			}
		}
	}
	if !hasWake {
		return nil
	}
	next := nextWake
	return &next
}

func (cs *CronService) Load() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.loadStore()
}

func (cs *CronService) SetOnJob(handler JobHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.onJob = handler
}

func (cs *CronService) loadStore() error {
	cs.store = &CronStore{
		Version: 1,
		Jobs:    []CronJob{},
	}

	data, err := os.ReadFile(cs.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if err := json.Unmarshal(data, cs.store); err != nil {
		if recoverErr := cs.recoverCorruptStore(err); recoverErr != nil {
			return recoverErr
		}
		return nil
	}
	return nil
}

func (cs *CronService) recoverCorruptStore(cause error) error {
	backupPath := fmt.Sprintf("%s.corrupt.%d", cs.storePath, time.Now().UnixMilli())
	if err := os.Rename(cs.storePath, backupPath); err != nil {
		return fmt.Errorf("recover corrupt cron store: %w (original parse error: %v)", err, cause)
	}
	log.Printf("[cron] recovered corrupt store %s (backup: %s): %v", cs.storePath, backupPath, cause)
	cs.store = &CronStore{
		Version: 1,
		Jobs:    []CronJob{},
	}
	return nil
}

func (cs *CronService) saveStoreUnsafe() error {
	dir := filepath.Dir(cs.storePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cs.store, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(cs.storePath, data, 0600)
}

func (cs *CronService) AddJob(name string, schedule CronSchedule, message string, deliver bool, channel, to string) (*CronJob, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	now := time.Now().UnixMilli()
	schedule = normalizeSchedule(schedule)
	if err := cs.validateSchedule(schedule, now, true); err != nil {
		return nil, err
	}

	// One-time tasks (at) should be deleted after execution
	deleteAfterRun := (schedule.Kind == "at")

	job := CronJob{
		ID:       generateID(),
		Name:     name,
		Enabled:  true,
		Schedule: schedule,
		Payload: CronPayload{
			Kind:    "agent_turn",
			Message: message,
			Deliver: deliver,
			Channel: channel,
			To:      to,
		},
		State: CronJobState{
			NextRunAtMS: cs.computeNextRun(&schedule, now),
		},
		CreatedAtMS:    now,
		UpdatedAtMS:    now,
		DeleteAfterRun: deleteAfterRun,
	}

	cs.store.Jobs = append(cs.store.Jobs, job)
	if err := cs.saveStoreUnsafe(); err != nil {
		return nil, err
	}

	jobCopy := cloneJob(job)
	return &jobCopy, nil
}

func (cs *CronService) UpdateJob(job *CronJob) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()

	nextJob := cloneJob(*job)
	nextJob.Schedule = normalizeSchedule(nextJob.Schedule)
	if err := cs.validateSchedule(nextJob.Schedule, time.Now().UnixMilli(), false); err != nil {
		return err
	}
	if strings.TrimSpace(nextJob.Payload.Kind) == "" {
		nextJob.Payload.Kind = "agent_turn"
	}

	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID == nextJob.ID {
			now := time.Now().UnixMilli()
			nextJob.CreatedAtMS = cs.store.Jobs[i].CreatedAtMS
			nextJob.UpdatedAtMS = now
			if nextJob.Enabled {
				nextJob.State.NextRunAtMS = cs.computeNextRun(&nextJob.Schedule, now)
				if nextJob.State.NextRunAtMS == nil {
					nextJob.Enabled = false
					nextJob.State.LastStatus = "error"
					nextJob.State.LastError = "schedule does not have a future run"
				}
			} else {
				nextJob.State.NextRunAtMS = nil
			}
			cs.store.Jobs[i] = nextJob
			return cs.saveStoreUnsafe()
		}
	}
	return fmt.Errorf("job not found")
}

func (cs *CronService) RemoveJob(jobID string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	return cs.removeJobUnsafe(jobID)
}

func (cs *CronService) removeJobUnsafe(jobID string) bool {
	before := len(cs.store.Jobs)
	var jobs []CronJob
	for _, job := range cs.store.Jobs {
		if job.ID != jobID {
			jobs = append(jobs, job)
		}
	}
	cs.store.Jobs = jobs
	removed := len(cs.store.Jobs) < before

	if removed {
		if err := cs.saveStoreUnsafe(); err != nil {
			log.Printf("[cron] failed to save store after remove: %v", err)
		}
	}

	return removed
}

func (cs *CronService) EnableJob(jobID string, enabled bool) *CronJob {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if job.ID == jobID {
			job.Enabled = enabled
			job.UpdatedAtMS = time.Now().UnixMilli()

			if enabled {
				job.State.NextRunAtMS = cs.computeNextRun(&job.Schedule, time.Now().UnixMilli())
				if job.State.NextRunAtMS == nil {
					job.Enabled = false
					job.State.LastStatus = "error"
					job.State.LastError = "schedule does not have a future run"
				}
			} else {
				job.State.NextRunAtMS = nil
			}

			if err := cs.saveStoreUnsafe(); err != nil {
				log.Printf("[cron] failed to save store after enable: %v", err)
			}
			jobCopy := cloneJob(*job)
			return &jobCopy
		}
	}

	return nil
}

func (cs *CronService) ListJobs(includeDisabled bool) []CronJob {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if includeDisabled {
		return cloneJobs(cs.store.Jobs)
	}

	var enabled []CronJob
	for _, job := range cs.store.Jobs {
		if job.Enabled {
			enabled = append(enabled, cloneJob(job))
		}
	}

	return enabled
}

func (cs *CronService) Status() map[string]interface{} {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	var enabledCount int
	for _, job := range cs.store.Jobs {
		if job.Enabled {
			enabledCount++
		}
	}

	return map[string]interface{}{
		"enabled":      cs.running,
		"jobs":         len(cs.store.Jobs),
		"nextWakeAtMS": cs.getNextWakeMS(),
		"enabledJobs":  enabledCount,
	}
}

func cloneJobs(jobs []CronJob) []CronJob {
	out := make([]CronJob, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, cloneJob(job))
	}
	return out
}

func cloneInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

func cloneJob(job CronJob) CronJob {
	out := job
	out.Schedule.AtMS = cloneInt64Ptr(job.Schedule.AtMS)
	out.Schedule.EveryMS = cloneInt64Ptr(job.Schedule.EveryMS)
	out.State.NextRunAtMS = cloneInt64Ptr(job.State.NextRunAtMS)
	out.State.LastRunAtMS = cloneInt64Ptr(job.State.LastRunAtMS)
	return out
}

func generateID() string {
	// Use crypto/rand for better uniqueness under concurrent access
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based if crypto/rand fails
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
