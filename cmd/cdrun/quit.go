package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func quitSession(ep endpoint) error {
	if ep.Native {
		empty := ""
		_, err := fetch(ep, "/archive", &empty)
		return err
	}
	h, err := openVerified(ep, false)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	exit := "/exit"
	if _, err := fetch(ep, "/prompt", &exit); err != nil {
		return err
	}
	grace := time.Now().Add(10 * time.Second)
	deadline := time.Now().Add(8 * time.Minute)
	forced := false
	for time.Now().Before(deadline) {
		result, err := windows.WaitForSingleObject(h, 200)
		if err != nil {
			return err
		}
		if result == windows.WAIT_OBJECT_0 {
			data, err := os.ReadFile(ep.FinalPath)
			if err != nil {
				return fmt.Errorf("session %d exited without a confirmed archive: %w", ep.PID, err)
			}
			var s snapshot
			if json.Unmarshal(data, &s) != nil || !s.Archived {
				return fmt.Errorf("session %d closed, but conversation not archived: %s", ep.PID, s.Error)
			}
			fmt.Fprintf(os.Stderr, "session %d exited and archived\n", ep.PID)
			return nil
		}
		if !forced && time.Now().After(grace) {
			// Stop work, but let the broker finish releasing its writer and archiving.
			empty := ""
			_, _ = fetch(ep, "/force-close", &empty)
			forced = true
		}
	}
	return fmt.Errorf("session %d has not confirmed archive; broker left running to finish shutdown", ep.PID)
}
