//go:build windows

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

func TestEndpointIdentityAndLocalAddress(t *testing.T) {
	exe, _ := os.Executable()
	birth, _ := processBirth(windows.CurrentProcess())
	e := endpoint{PID: os.Getpid(), Created: birth, Executable: exe, URL: "http://127.0.0.1:1234", Token: strings.Repeat("a", 64)}
	if err := validateEndpoint(e, e.PID); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*endpoint){
		func(e *endpoint) { e.Created++ },
		func(e *endpoint) { e.Executable = `C:\unrelated.exe` },
		func(e *endpoint) { e.URL = "http://example.com:1234" },
		func(e *endpoint) { e.URL = "http://127.0.0.1:1234/other" },
		func(e *endpoint) { e.Token = "short" },
	} {
		bad := e
		mutate(&bad)
		if err := validateEndpoint(bad, e.PID); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestPrivateStateACL(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if err := secureStateRoot(); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(stateRoot(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	if !strings.Contains(sddl, "D:P") || !strings.Contains(sddl, "NR") || !strings.Contains(sddl, "NW") {
		t.Fatal(sddl)
	}
	if err := secureStateRoot(); err != nil {
		t.Fatal("existing root", err)
	}
}

func TestLimitsPreserveUTF8(t *testing.T) {
	v := turn{}
	appendAnswer(&v, strings.Repeat("你", maxAnswerBytes))
	if !v.Truncated || len(v.Answer) > maxAnswerBytes || !utf8.ValidString(v.Answer) {
		t.Fatal("invalid truncation")
	}
	appendAnswer(&v, "additional")
	if len(v.Answer) > maxAnswerBytes {
		t.Fatal("limit bypass")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &cappedWriter{file: f, limit: 8}
	if n, err := w.Write([]byte("1234567890")); n != 10 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := w.Write([]byte("more")); n != 4 || err != nil {
		t.Fatal(n, err)
	}
	info, _ := f.Stat()
	if info.Size() != 8 {
		t.Fatal(info.Size())
	}
}

func TestStrictIPC(t *testing.T) {
	b := &broker{state: snapshot{Status: "idle"}, queue: make(chan string, 1), stop: make(chan struct{})}
	h := b.handler(endpoint{Token: "secret"})
	for _, body := range []string{`{"Prompt":"hello","Typo":true}`, `{"Prompt":"hello"} {}`, `{"Prompt":""}`} {
		r := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code, body)
		}
	}
	b.state.Error = "server failed"
	r := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(`{"Prompt":"/exit"}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h(w, r)
	select {
	case <-b.stop:
	default:
		t.Fatal("failed broker cannot close")
	}
}

func TestDelayedAndDuplicateEvents(t *testing.T) {
	b := &broker{state: snapshot{ThreadID: "t", Turns: []turn{{ID: "new", Status: "inProgress"}}}, finished: make(chan turn, 1)}
	b.event(wire{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"t","turnId":"old","delta":"wrong"}`)})
	w := wire{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"t","turn":{"id":"new","status":"completed"}}`)}
	b.event(w)
	b.event(w)
	if len(b.finished) != 1 || b.state.Turns[0].Answer != "" {
		t.Fatal("duplicate or stale event applied")
	}
}

func TestSkillBridge(t *testing.T) {
	for _, tc := range []struct {
		prompt, result string
		bad            bool
	}{
		{"/example-skill -go", `{"data":[{"skills":[{"name":"example-skill","path":"C:/skills/example-skill/SKILL.md","enabled":true}]}]}`, false},
		{"/review inspect changes", `{"data":[{"skills":[{"name":"review","path":"C:/skills/review/SKILL.md","enabled":true}]}]}`, false},
		{"/review", `{"data":[{"skills":[{"name":"other","path":"x","enabled":true}]}]}`, true},
		{"/review", `{"data":[{"skills":[{"name":"review","path":"x","enabled":true},{"name":"review","path":"y","enabled":true}]}]}`, true},
		{"/example-skill -done", `{"data":[{"skills":[]}]}`, true},
		{"/example-skill", `{"data":[{"skills":[{"name":"example-skill","path":"x","enabled":false}]}]}`, true},
	} {
		reader, writer := io.Pipe()
		r := &rpc{in: writer, pending: make(map[string]chan wire), dead: make(chan struct{})}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer reader.Close()
			var w wire
			if json.NewDecoder(reader).Decode(&w) != nil {
				return
			}
			r.mu.Lock()
			ch := r.pending[string(w.ID)]
			r.mu.Unlock()
			ch <- wire{Result: json.RawMessage(tc.result)}
		}()
		input, err := skillInput(r, `C:\workspace`, tc.prompt)
		writer.Close()
		<-done
		if (err != nil) != tc.bad {
			t.Fatal(tc.prompt, err)
		}
		if !tc.bad && (len(input) != 2 || input[0].(object)["text"] != tc.prompt || input[1].(object)["type"] != "skill") {
			t.Fatal(input)
		}
		if !tc.bad && input[1].(object)["name"] != strings.TrimPrefix(strings.Fields(tc.prompt)[0], "/") {
			t.Fatal("skill name changed", input)
		}
	}
}

func TestOrdinaryPromptsDoNotDiscoverSkills(t *testing.T) {
	for _, prompt := range []string{"", "Review the code", "/path/to/file contains data", "https://example.com", "Use /review later"} {
		input, err := skillInput(nil, `C:\workspace`, prompt)
		if err != nil || len(input) != 1 || input[0].(object)["text"] != prompt {
			t.Fatal(prompt, input, err)
		}
	}
}

func TestBrokerContinuesAfterMissingSkill(t *testing.T) {
	b := &broker{state: snapshot{Status: "idle"}, queue: make(chan string, 2), stop: make(chan struct{}), inputs: func(string) ([]any, error) { return nil, os.ErrNotExist }}
	b.queue <- "/example-skill"
	b.queue <- "/exit"
	b.state.Queued = 2
	go b.run()
	select {
	case <-b.stop:
	case <-time.After(time.Second):
		t.Fatal("queue stuck after missing skill")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Completed != 1 || b.state.Turns[0].Status != "failed" || b.state.Queued != 0 {
		t.Fatal(b.state)
	}
}

func TestBoundedHistoryAndSequence(t *testing.T) {
	b := &broker{state: snapshot{Status: "idle"}, queue: make(chan string, 32), stop: make(chan struct{}), inputs: func(string) ([]any, error) { return nil, os.ErrNotExist }}
	for i := 0; i < 20; i++ {
		b.queue <- "/example-skill"
	}
	b.queue <- "/exit"
	b.state.Queued = 21
	go b.run()
	select {
	case <-b.stop:
	case <-time.After(time.Second):
		t.Fatal("queue stuck")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Completed != 20 || len(b.state.Turns) != maxTurns || b.state.FirstSequence != 5 || b.state.Turns[maxTurns-1].Sequence != 20 {
		t.Fatal(b.state)
	}
}

func TestCancelSendsCurrentTurnIdentity(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	rpcClient := &rpc{in: writer, pending: make(map[string]chan wire), dead: make(chan struct{})}
	b := &broker{r: rpcClient, state: snapshot{ThreadID: "thread", Turns: []turn{{ID: "turn", Status: "inProgress"}}}}
	got := make(chan wire, 1)
	go func() {
		var w wire
		if json.NewDecoder(reader).Decode(&w) != nil {
			return
		}
		got <- w
		rpcClient.mu.Lock()
		ch := rpcClient.pending[string(w.ID)]
		rpcClient.mu.Unlock()
		ch <- wire{Result: json.RawMessage(`{}`)}
	}()
	req := httptest.NewRequest("POST", "/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	b.handler(endpoint{Token: "secret"})(response, req)
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	w := <-got
	var params struct{ ThreadID, TurnID string }
	_ = json.Unmarshal(w.Params, &params)
	if w.Method != "turn/interrupt" || params.ThreadID != "thread" || params.TurnID != "turn" {
		t.Fatal(w.Method, params)
	}
}

func TestFatalStartDoesNotReplayQueuedWork(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r := &rpc{in: writer, pending: make(map[string]chan wire), dead: make(chan struct{})}
	b := &broker{r: r, state: snapshot{Status: "idle", Queued: 2}, queue: make(chan string, 2), stop: make(chan struct{})}
	b.queue <- "first"
	b.queue <- "second"
	go func() {
		var w wire
		if json.NewDecoder(reader).Decode(&w) != nil {
			return
		}
		r.mu.Lock()
		ch := r.pending[string(w.ID)]
		r.mu.Unlock()
		ch <- wire{Error: json.RawMessage(`{"code":-1,"message":"failure"}`)}
	}()
	done := make(chan struct{})
	go func() { b.run(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fatal start kept running")
	}
	if b.state.Completed != 1 || b.state.Queued != 0 || b.state.Error == "" || len(b.state.Turns) != 1 {
		t.Fatal(b.state)
	}
}
