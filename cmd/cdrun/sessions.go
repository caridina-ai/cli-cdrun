package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/sys/windows"
)

type runningSession struct {
	endpoint
	Status, ThreadID string
}

// Discover only brokers whose process identity and authenticated endpoint agree.
func brokerSessions() ([]runningSession, error) {
	paths, err := filepath.Glob(filepath.Join(stateRoot(), "*.json"))
	if err != nil {
		return nil, err
	}
	var found []runningSession
	for _, path := range paths {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil {
			continue
		}
		e, err := loadEndpoint(pid)
		if err != nil {
			continue
		}
		data, err := fetch(e, "/status", nil)
		if err != nil {
			if e.Native {
				active, lockErr := nativeWriterActive(e.ThreadID)
				if lockErr != nil {
					return nil, lockErr
				}
				if active {
					return nil, fmt.Errorf("read native proxy %d: %w", e.PID, err)
				}
			}
			continue
		}
		var s snapshot
		if json.Unmarshal(data, &s) != nil || s.PID != e.PID {
			continue
		}
		e.Name, e.Dir = s.Name, s.Cwd
		found = append(found, runningSession{e, s.Status, s.ThreadID})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Created < found[j].Created })
	return found, nil
}

func runningSessions() ([]runningSession, error) {
	if err := discoverNativeSessions(); err != nil {
		return nil, err
	}
	return brokerSessions()
}

func sessionsNamed(name string) ([]runningSession, error) {
	all, err := runningSessions()
	var found []runningSession
	for _, s := range all {
		if strings.EqualFold(name, s.Name) {
			found = append(found, s)
		}
	}
	return found, err
}

func ambiguousName(name string, found []runningSession) error {
	pids := make([]string, len(found))
	for i, s := range found {
		pids[i] = strconv.Itoa(s.PID)
	}
	return fmt.Errorf("%d running sessions are named %q (pids %s); pick one with -p", len(found), name, strings.Join(pids, ", "))
}

func pidByName(name string) (int, error) {
	found, err := sessionsNamed(name)
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, fmt.Errorf("no running session is named %q; cdrun -l lists them", name)
	}
	if len(found) > 1 {
		return 0, ambiguousName(name, found)
	}
	return found[0].PID, nil
}

func sameDirectory(a, b string) bool {
	return strings.EqualFold(filepath.Clean(strings.ReplaceAll(a, "/", `\`)), filepath.Clean(strings.ReplaceAll(b, "/", `\`)))
}

func startOrReuse(dir, name, prompt string, trust bool) error {
	// Windows mutex ownership belongs to an OS thread, not a Go goroutine.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(abs), ":")
		if name == `\` || name == "." || name == "" {
			name = "cdrun"
		}
	}
	// Serialize discovery and publication so concurrent scheduled launches cannot duplicate names.
	key := fmt.Sprintf(`Local\cdrun-launch-%x`, sha256.Sum256([]byte(strings.ToLower(stateRoot()+"|"+name))))
	mutexName, _ := windows.UTF16PtrFromString(key)
	h, err := windows.CreateMutex(nil, false, mutexName)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	defer windows.CloseHandle(h)
	result, err := windows.WaitForSingleObject(h, 120000)
	if err != nil {
		return err
	}
	if result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED {
		return fmt.Errorf("timed out waiting to start session %q", name)
	}
	defer windows.ReleaseMutex(h)
	unlock, err := lockRegistry()
	if err != nil {
		return err
	}
	defer unlock()
	found, err := sessionsNamed(name)
	if err != nil {
		return err
	}
	if len(found) > 1 {
		return ambiguousName(name, found)
	}
	if len(found) == 1 {
		s := found[0]
		if !sameDirectory(abs, s.Dir) {
			return fmt.Errorf("session %q is already running in %s (pid %d); no new session started", name, s.Dir, s.PID)
		}
		if prompt != "" {
			if _, err := fetch(s.endpoint, "/prompt", &prompt); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "already running in %s as %q (pid %d); no new session started\n", s.Dir, s.Name, s.PID)
		fmt.Println(s.PID)
		return nil
	}
	return launch(abs, name, prompt, trust)
}

func (s runningSession) matches(keywords []string) bool {
	for _, keyword := range keywords {
		matched := false
		for _, field := range []string{s.Name, s.Dir, s.ThreadID} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(keyword)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
			continue
		}
		n++
		if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff01 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 || r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3fffd) {
			n++
		}
	}
	return n
}

func listSessions(keywords []string) error {
	all, err := runningSessions()
	if err != nil {
		return err
	}
	rows := [][]string{{"PID", "NAME", "STATUS", "DIRECTORY"}}
	for _, s := range all {
		if s.matches(keywords) {
			rows = append(rows, []string{strconv.Itoa(s.PID), s.Name, s.Status, s.Dir})
		}
	}
	if len(rows) == 1 {
		if len(keywords) == 0 {
			return fmt.Errorf("no Codex session is running")
		}
		quoted := make([]string, len(keywords))
		for i, k := range keywords {
			quoted[i] = strconv.Quote(k)
		}
		return fmt.Errorf("no running session matches %s", strings.Join(quoted, " "))
	}
	widths := make([]int, 4)
	for _, row := range rows {
		for j, cell := range row {
			widths[j] = max(widths[j], displayWidth(cell))
		}
	}
	for i, row := range rows {
		var line strings.Builder
		for j, cell := range row {
			line.WriteString(cell)
			if j < 3 {
				line.WriteString(strings.Repeat(" ", widths[j]-displayWidth(cell)+2))
			}
		}
		out := os.Stdout
		if i == 0 {
			out = os.Stderr
		}
		fmt.Fprintln(out, line.String())
	}
	if len(keywords) > 0 && len(rows) > 2 {
		fmt.Fprintf(os.Stderr, "%d sessions match; add or lengthen keywords to leave one\n", len(rows)-1)
	}
	return nil
}
