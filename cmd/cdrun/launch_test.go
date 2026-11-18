package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestLaunchHelperInStrictJob runs inside a Job Object that forbids breakaway, like a
// sandboxed or job-managed shell, and launches a child that exits at once.
func TestLaunchHelperInStrictJob(t *testing.T) {
	if os.Getenv("CDRUN_LAUNCH_TEST_HELPER") != "1" {
		t.Skip("helper process only")
	}
	// A job without JOB_OBJECT_LIMIT_BREAKAWAY_OK denies CREATE_BREAKAWAY_FROM_JOB. Its handle
	// stays open until this helper exits; no kill-on-close limit, so the helper reports first.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(stateRoot(), 0700); err != nil {
		t.Fatal(err)
	}
	// The child is this test binary with no tests to run, so it exits before publishing
	// an endpoint; reaching that point proves the process was started.
	err = launchProcess([]string{"-test.run=^$"}, "", false, false)
	if err == nil || !strings.Contains(err.Error(), "session startup failed") {
		t.Fatalf("launch inside a job without breakaway: %v", err)
	}
}

func TestLaunchFallsBackWhenBreakawayIsDenied(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLaunchHelperInStrictJob$", "-test.v")
	cmd.Env = append(os.Environ(), "CDRUN_LAUNCH_TEST_HELPER=1", "LOCALAPPDATA="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestLaunchHelperInStrictJob") {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
}
