package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// quitArgs permits -q after a prompt without interpreting other prompt words as flags.
// Flag values and everything after -- retain their literal meaning.
func quitArgs(args []string) []string {
	var flags, rest []string
	positional := false
	for i := 0; i < len(args); i++ {
		s := args[i]
		if s == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if s == "-q" || s == "--q" || strings.HasPrefix(s, "-q=") || strings.HasPrefix(s, "--q=") {
			flags = append(flags, s)
			continue
		}
		rest = append(rest, s)
		if !positional {
			switch s {
			case "-d", "--d", "-n", "--n", "-p", "--p":
				if i+1 < len(args) {
					i++
					rest = append(rest, args[i])
				}
			default:
				positional = !strings.HasPrefix(s, "-") || s == "-"
			}
		}
	}
	return append(flags, rest...)
}

// submitPrompt marks draining in the same broker request that accepts the last prompt.
func submitPrompt(ep endpoint, prompt string, quit bool) error {
	path := "/prompt"
	if quit {
		if ep.Native && prompt != "" {
			return fmt.Errorf("prompt with -q requires a cdrun-owned session; send the prompt without -q and archive in its Codex app or CLI")
		}
		if prompt == "" {
			prompt = "/exit"
		} else {
			path = "/prompt-and-exit"
		}
	}
	_, err := fetch(ep, path, &prompt)
	return err
}

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
	// The broker finishes every accepted turn first, however long that takes.
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		return err
	}
	data, err := os.ReadFile(ep.FinalPath)
	if err != nil {
		return fmt.Errorf("session %d exited without a confirmed archive: %w", ep.PID, err)
	}
	var s snapshot
	if json.Unmarshal(data, &s) != nil {
		return fmt.Errorf("session %d exited without a readable result", ep.PID)
	}
	switch {
	case s.Archived:
		fmt.Fprintf(os.Stderr, "session %d exited and archived\n", ep.PID)
	case s.LeftOpen:
		// The Codex app keeps a conversation it ran open until it quits; only it can archive it now.
		fmt.Fprintf(os.Stderr, "session %d exited; the Codex app still has its conversation open, so archive it in the app\n", ep.PID)
	default:
		return fmt.Errorf("session %d closed, but conversation not archived: %s", ep.PID, s.Error)
	}
	return nil
}
