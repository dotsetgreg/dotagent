package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateSnapshots = flag.Bool("update", false, "update CLI help snapshots")

type helpSnapshotCase struct {
	name     string
	args     []string
	snapshot string
}

func TestCLIHelpSnapshots(t *testing.T) {
	t.Parallel()

	cases := []helpSnapshotCase{
		{
			name:     "root_help",
			args:     []string{"--help"},
			snapshot: "root_help.txt",
		},
		{
			name:     "cron_help",
			args:     []string{"cron", "--help"},
			snapshot: "cron_help.txt",
		},
		{
			name:     "toolpacks_help",
			args:     []string{"toolpacks", "--help"},
			snapshot: "toolpacks_help.txt",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output, err := runRootCommandForTest(tc.args...)
			if err != nil {
				t.Fatalf("execute command %v: %v\nOutput:\n%s", tc.args, err, output)
			}

			snapshotPath := filepath.Join("testdata", "cli", tc.snapshot)
			if *updateSnapshots {
				if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o755); err != nil {
					t.Fatalf("mkdir snapshot dir: %v", err)
				}
				if err := os.WriteFile(snapshotPath, []byte(output), 0o644); err != nil {
					t.Fatalf("write snapshot: %v", err)
				}
			}

			expected, readErr := os.ReadFile(snapshotPath)
			if readErr != nil {
				t.Fatalf("read snapshot %s: %v", snapshotPath, readErr)
			}
			if output != string(expected) {
				t.Fatalf("snapshot mismatch for %s\n--- expected ---\n%s\n--- actual ---\n%s", tc.name, string(expected), output)
			}
		})
	}
}

func runRootCommandForTest(args ...string) (string, error) {
	root := buildRootCommand(false)
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}
