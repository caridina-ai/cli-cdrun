package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func postPrompt(b *broker, path, prompt string) int {
	body, _ := json.Marshal(request{Prompt: prompt})
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	b.handler(endpoint{Token: "secret"})(w, r)
	return w.Code
}

func TestPromptAndQuitDrainsAllWork(t *testing.T) {
	b, _ := newIdleBroker()
	b.queue = make(chan string, 2)
	firstStarted, finishFirst := make(chan struct{}), make(chan struct{})
	f := newFakeServer(t, b, func(method string) (string, string) {
		if method == "turn/start" {
			select {
			case <-firstStarted:
			default:
				close(firstStarted)
				<-finishFirst
			}
		}
		return `{}`, ""
	})
	b.r, b.holding = f.r, true
	defer b.shutdown()
	if postPrompt(b, "/prompt", "first") != 200 {
		t.Fatal("first prompt rejected")
	}
	done := make(chan struct{})
	go func() { b.run(); close(done) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	if postPrompt(b, "/prompt", "second") != 200 || postPrompt(b, "/prompt-and-exit", "last") != 200 {
		t.Fatal("could not queue remaining work")
	}
	if postPrompt(b, "/prompt", "too late") != 409 || postPrompt(b, "/prompt", "/exit") != 200 {
		t.Fatal("draining must reject new work and accept repeated quit")
	}
	select {
	case <-b.stop:
		t.Fatal("quit interrupted work")
	default:
	}
	close(finishFirst)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broker did not drain")
	}
	if b.state.Completed != 3 || b.state.Queued != 0 || b.state.Status != "closed" {
		t.Fatal(b.state)
	}
	for i, prompt := range []string{"first", "second", "last"} {
		if b.state.Turns[i].Prompt != prompt {
			t.Fatal(b.state.Turns)
		}
	}
}

func TestQuitWithFullQueue(t *testing.T) {
	b, _ := newIdleBroker()
	b.queue = make(chan string, 1)
	if postPrompt(b, "/prompt", "first") != 200 {
		t.Fatal("prompt rejected")
	}
	if postPrompt(b, "/prompt-and-exit", "last") != 429 || b.closing || b.state.Queued != 1 {
		t.Fatal("full queue partially accepted prompt or quit")
	}
	if postPrompt(b, "/prompt", "/exit") != 200 || !b.closing || b.state.Queued != 1 {
		t.Fatal("quit requires a free queue slot")
	}
	b.inputs = func(string) ([]any, error) { return nil, os.ErrNotExist }
	go b.run()
	select {
	case <-b.stop:
	case <-time.After(time.Second):
		t.Fatal("full queue did not drain")
	}
}

func TestQuitDoesNotInterruptTurnWithReportedError(t *testing.T) {
	b, _ := newIdleBroker()
	b.state.Status = "running"
	b.state.Error = "Unsupported server request"
	if postPrompt(b, "/prompt", "/exit") != 200 {
		t.Fatal("quit rejected")
	}
	select {
	case <-b.stop:
		t.Fatal("a reported error caused quit to interrupt an active turn")
	default:
	}
	if !b.closing || <-b.queue != "/exit" {
		t.Fatal("quit did not wait behind the active turn")
	}
}

func TestCLICombinedQuitRouting(t *testing.T) {
	dir := t.TempDir()
	ep := fixtureSession(t, "fixture", dir)
	b, _ := newIdleBroker()
	s := httptest.NewServer(b.handler(endpoint{Token: ep.Token}))
	defer s.Close()
	ep.URL = s.URL
	for _, args := range [][]string{
		{"-p", strconv.Itoa(ep.PID), "hello", "-q"},
		{"-p", strconv.Itoa(ep.PID), "-q", "hello"},
		{"-n", "fixture", "hello", "-q"},
		{"-d", dir, "-n", "fixture", "hello", "-q"},
		{"hello", "-d", dir, "-n", "fixture", "-q", "--"},
	} {
		b.state = snapshot{PID: ep.PID, Cwd: dir, Name: "fixture", Status: "idle"}
		b.closing = false
		if err := writeJSON(endpointPath(ep.PID), ep); err != nil {
			t.Fatal(err)
		}
		if err := run(args); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		if !b.closing || <-b.queue != "hello" {
			t.Fatal("prompt and quit were not accepted together")
		}
	}
	b.closing = false
	input, err := os.CreateTemp(t.TempDir(), "stdin-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("本尊\nsecond"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = original }()
	if err := run([]string{"-p", strconv.Itoa(ep.PID), "-", "-q"}); err != nil {
		t.Fatal(err)
	}
	if !b.closing || <-b.queue != "本尊\nsecond" {
		t.Fatal("UTF-8 stdin prompt changed")
	}
}

func TestNativeCombinedQuitRejectsBeforeInjection(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("native prompt was partially sent") }))
	defer s.Close()
	if err := submitPrompt(endpoint{Native: true, URL: s.URL}, "hello", true); err == nil {
		t.Fatal("native combined quit silently accepted")
	}
}

func TestQuitWaitHelper(t *testing.T) {
	if os.Getenv("CDRUN_QUIT_TEST_HELPER") != "1" {
		return
	}
	result := snapshot{Archived: true}
	if os.Getenv("CDRUN_QUIT_TEST_LEFT_OPEN") == "1" {
		result = snapshot{LeftOpen: true}
	} else {
		time.Sleep(11 * time.Second)
	}
	if err := writeJSON(os.Getenv("CDRUN_QUIT_TEST_FINAL"), result); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestQuitWaitsBeyondOldForceDeadline(t *testing.T) {
	quitThroughHelper(t)
}

func TestQuitReturnsWhenTheAppKeepsTheConversation(t *testing.T) {
	quitThroughHelper(t, "CDRUN_QUIT_TEST_LEFT_OPEN=1")
}

// quitThroughHelper quits a stand-in broker process that writes its result when it exits.
func quitThroughHelper(t *testing.T, env ...string) {
	t.Helper()
	final := filepath.Join(t.TempDir(), "final.json")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestQuitWaitHelper$")
	cmd.Env = append(append(os.Environ(), "CDRUN_QUIT_TEST_HELPER=1", "CDRUN_QUIT_TEST_FINAL="+final), env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	birth, err := processBirth(h)
	windows.CloseHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading status is harmless; anything else but the queued /exit could cut work short.
		if r.URL.Path != "/prompt" && !(r.Method == http.MethodGet && r.URL.Path == "/status") {
			t.Errorf("quit interrupted work via %s", r.URL.Path)
		}
		w.Write([]byte(`{}`))
	}))
	defer s.Close()
	if err := quitSession(endpoint{PID: cmd.Process.Pid, Created: birth, Executable: exe, URL: s.URL, FinalPath: final}); err != nil {
		t.Fatal(err)
	}
}
