package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSaveStore_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not enforced on Windows")
	}

	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")

	cs := NewCronService(storePath, nil)

	_, err := cs.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "hello", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	info, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("cron store has permission %04o, want 0600", perm)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

func TestAddJob_RejectsInvalidSchedules(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")
	cs := NewCronService(storePath, nil)

	tests := []struct {
		name     string
		schedule CronSchedule
	}{
		{
			name:     "invalid cron expression",
			schedule: CronSchedule{Kind: "cron", Expr: "invalid expression"},
		},
		{
			name:     "non-positive every interval",
			schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(0)},
		},
		{
			name:     "one-time in the past",
			schedule: CronSchedule{Kind: "at", AtMS: int64Ptr(time.Now().UnixMilli() - 1000)},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := cs.AddJob("test", tc.schedule, "msg", false, "cli", "direct")
			if err == nil {
				t.Fatalf("expected invalid schedule error")
			}
		})
	}
}

func TestCronService_ListJobsReturnsCopies(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")
	cs := NewCronService(storePath, nil)

	job, err := cs.AddJob("immutable", CronSchedule{Kind: "every", EveryMS: int64Ptr(1000)}, "hello", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	listed := cs.ListJobs(true)
	if len(listed) != 1 {
		t.Fatalf("expected one job, got %d", len(listed))
	}
	listed[0].Name = "mutated"
	if listed[0].Schedule.EveryMS == nil {
		t.Fatalf("expected every schedule")
	}
	*listed[0].Schedule.EveryMS = 42

	again := cs.ListJobs(true)
	if again[0].Name == "mutated" {
		t.Fatalf("job name mutation leaked into service state")
	}
	if again[0].Schedule.EveryMS == nil || *again[0].Schedule.EveryMS != 1000 {
		t.Fatalf("job schedule mutation leaked into service state")
	}

	disabled := cs.EnableJob(job.ID, false)
	if disabled == nil {
		t.Fatalf("expected job from EnableJob")
	}
	disabled.Name = "changed-via-enable"

	afterEnable := cs.ListJobs(true)
	if afterEnable[0].Name == "changed-via-enable" {
		t.Fatalf("EnableJob returned internal pointer instead of copy")
	}
}

func TestEnableJob_ExpiredOneTimeJobRemainsDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")
	cs := NewCronService(storePath, nil)

	atMS := time.Now().UnixMilli() + 30_000
	job, err := cs.AddJob("one-shot", CronSchedule{Kind: "at", AtMS: &atMS}, "hello", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	past := time.Now().UnixMilli() - 5_000
	job.Schedule.AtMS = &past
	if err := cs.UpdateJob(job); err != nil {
		t.Fatalf("UpdateJob failed: %v", err)
	}

	out := cs.EnableJob(job.ID, true)
	if out == nil {
		t.Fatalf("expected EnableJob response")
	}
	if out.Enabled {
		t.Fatalf("expected expired one-time job to remain disabled")
	}
	if !strings.Contains(strings.ToLower(out.State.LastError), "future run") {
		t.Fatalf("expected future run error, got %q", out.State.LastError)
	}
}

func TestStatus_NextWakePointerIsDetached(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")
	cs := NewCronService(storePath, nil)

	_, err := cs.AddJob("status", CronSchedule{Kind: "every", EveryMS: int64Ptr(60_000)}, "hello", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	statusA := cs.Status()
	ptrA, ok := statusA["nextWakeAtMS"].(*int64)
	if !ok || ptrA == nil {
		t.Fatalf("expected nextWakeAtMS pointer in status")
	}
	*ptrA = 1

	statusB := cs.Status()
	ptrB, ok := statusB["nextWakeAtMS"].(*int64)
	if !ok || ptrB == nil {
		t.Fatalf("expected nextWakeAtMS pointer in status")
	}
	if *ptrB == 1 {
		t.Fatalf("status exposed mutable internal pointer")
	}
}
