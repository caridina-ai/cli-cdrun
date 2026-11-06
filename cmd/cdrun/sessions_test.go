package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func fixtureSession(t *testing.T, name, dir string) endpoint {
	t.Helper()
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	if err := secureStateRoot(); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			json.NewEncoder(w).Encode(snapshot{PID: os.Getpid(), Name: name, Cwd: dir, Status: "released", ThreadID: "thread-fixture"})
		} else {
			json.NewEncoder(w).Encode(object{"accepted": true})
		}
	}))
	t.Cleanup(s.Close)
	exe, _ := os.Executable()
	birth, _ := processBirth(windows.CurrentProcess())
	e := endpoint{PID: os.Getpid(), Created: birth, Executable: exe, URL: s.URL, Token: strings.Repeat("a", 64), Name: name, Dir: dir}
	if err := writeJSON(endpointPath(e.PID), e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestNamesAndVerifiedDiscovery(t *testing.T) {
	e := fixtureSession(t, "00919", `D:\work\fund`)
	pid, err := pidByName("00919")
	if err != nil || pid != e.PID {
		t.Fatal(pid, err)
	}
	if _, err := pidByName("009"); err == nil || !strings.Contains(err.Error(), "cdrun -l") {
		t.Fatal(err)
	}
	e.Created++
	writeJSON(endpointPath(e.PID), e)
	if all, err := runningSessions(); err != nil || len(all) != 0 {
		t.Fatal(all, err)
	}
}

func TestReuseAndConflictWithoutCreatingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	e := fixtureSession(t, "name1", dir)
	if err := startOrReuse(strings.ToUpper(dir)+`\`, "NAME1", "hello", true, false); err != nil {
		t.Fatal(err)
	}
	if pid, err := pidByName("NAME1"); err != nil || pid != e.PID {
		t.Fatal(pid, err)
	}
	other := filepath.Join(t.TempDir(), "must-not-exist")
	if err := startOrReuse(other, "name1", "", true, false); err == nil || !strings.Contains(err.Error(), strconv.Itoa(e.PID)) {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("conflicting launch created directory", err)
	}
}

func TestReuseWithExistingWindowsMutex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	fixtureSession(t, "existing-lock", dir)
	key := fmt.Sprintf(`Local\cdrun-launch-%x`, sha256.Sum256([]byte(strings.ToLower(stateRoot()+"|existing-lock"))))
	mutexName, _ := windows.UTF16PtrFromString(key)
	h, err := windows.CreateMutex(nil, false, mutexName)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := startOrReuse(dir, "existing-lock", "", true, false); err != nil {
		t.Fatal("existing named mutex must be opened and waited on", err)
	}
}

func TestKeywordMatchingAndWindowsDirectories(t *testing.T) {
	s := runningSession{endpoint: endpoint{Name: "基金00919", Dir: `D:\work\fund`}, ThreadID: "thread-abc"}
	for _, tc := range []struct {
		keys []string
		want bool
	}{
		{nil, true}, {[]string{"009", "WORK", "ABC"}, true}, {[]string{"009", "missing"}, false},
	} {
		if s.matches(tc.keys) != tc.want {
			t.Fatal(tc)
		}
	}
	if !sameDirectory(`D:/work/fund`, `d:\work\fund\`) || sameDirectory(`D:\work\fund`, `D:\work\fund2`) {
		t.Fatal("directory comparison")
	}
	if displayWidth("基金00919") != 9 || displayWidth("e\u0301") != 1 {
		t.Fatal("display width")
	}
}

func TestCLIContractRejectsInvalidCombinations(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	for _, args := range [][]string{
		{"-r", "old"}, {"-d", "x", "-r", "old"}, {"-d", "x", "-s"},
		{"-p", "1", "-n", "name", "hi"}, {"-n", "name"}, {"-p", "1"}, {"-s"}, {"-q"},
		{"-n", "name", "-s", "hi"}, {"-n", "name", "-s", "-q"},
		{"-l", "-d", "x"}, {"-l", "-n", "x"}, {"-l", "-p", "1"}, {"-l", "-s"}, {"-l", "-q"},
		{"-n", "", "hi"}, {"-d", ""}, {"-n", "x", "--trust=false", "hi"},
	} {
		if err := run(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestArchiveNeverConfirmsFailureAndCanRetry(t *testing.T) {
	b, _ := newIdleBroker()
	attempts := 0
	f := newFakeServer(t, b, func(string) (string, string) {
		attempts++
		if attempts == 1 {
			return "", `{"code":-32600,"message":"temporary lock"}`
		}
		return `{}`, ""
	})
	b.connect = func() (*rpc, error) { return f.r, nil }
	b.archive()
	if b.state.Archived || !strings.Contains(b.state.Error, "not archived") {
		t.Fatal(b.state)
	}
	b.archive()
	if !b.state.Archived || b.state.Error != "" {
		t.Fatal(b.state)
	}
}

func TestArchiveRetryReleasesWriterAndReconnects(t *testing.T) {
	b, released := newIdleBroker()
	attempts := 0
	f := newFakeServer(t, b, func(method string) (string, string) {
		if method != "thread/archive" {
			t.Errorf("unexpected method: %s", method)
		}
		attempts++
		if attempts == 1 {
			return "", `{"code":-32600,"message":"temporary lock"}`
		}
		return `{}`, ""
	})
	connects := 0
	b.connect = func() (*rpc, error) { connects++; return f.r, nil }
	b.r, b.holding = f.r, true
	b.archiveWithRetry()
	if !b.state.Archived || b.state.Error != "" || *released != 2 || connects != 2 || b.holding {
		t.Fatal(b.state, *released, connects, b.holding)
	}
}
