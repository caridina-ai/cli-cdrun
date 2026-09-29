//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexDiscoveryWithoutAppPATH(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("PATH", root)
	t.Setenv("CDRUN_CODEX_EXE", "")
	exe := filepath.Join(root, "OpenAI", "Codex", "bin", "installed-version", "codex.exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("test fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCodex()
	if err != nil || got != exe {
		t.Fatalf("got %q, %v; want desktop installation", got, err)
	}
	t.Setenv("CDRUN_CODEX_EXE", filepath.Join(root, "missing.exe"))
	if _, err := resolveCodex(); err == nil {
		t.Fatal("invalid explicit override silently fell back")
	}
	t.Setenv("CDRUN_CODEX_EXE", exe)
	if got, err := resolveCodex(); err != nil || got != exe {
		t.Fatalf("explicit path failed: %q %v", got, err)
	}
}

func TestPromptUnicodeAndLimits(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{
		{"  本尊\r\n第二行\r第三行  ", "本尊\n第二行\n第三行", false},
		{strings.Repeat("你", 8000), strings.Repeat("你", 8000), false},
		{strings.Repeat("你", 8001), "", true},
		{"hello\x00world", "", true},
		{string([]byte{0xff}), "", true},
	} {
		got, err := normalize(tc.in)
		if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
			t.Fatalf("normalization failed: length=%d err=%v", len(tc.in), err)
		}
	}
}

func TestHTTPAuthorizationAndQueue(t *testing.T) {
	b := &broker{state: snapshot{Status: "idle"}, queue: make(chan string, 2), stop: make(chan struct{})}
	handler := b.handler(endpoint{Token: "test-secret"})
	send := func(body, auth string) int {
		req := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		recorder := httptest.NewRecorder()
		handler(recorder, req)
		return recorder.Code
	}
	if got := send(`{"Prompt":"unauthorized"}`, ""); got != 401 {
		t.Fatal(got)
	}
	if len(b.queue) != 0 {
		t.Fatal("unauthorized prompt entered queue")
	}
	if got := send(`{"Prompt":"本尊\nsecond"}`, "Bearer test-secret"); got != 200 {
		t.Fatal(got)
	}
	if got := send(`{"Prompt":"/exit"}`, "Bearer test-secret"); got != 200 {
		t.Fatal(got)
	}
	if got := send(`{"Prompt":"overflow"}`, "Bearer test-secret"); got != 409 {
		t.Fatal(got)
	}
	if <-b.queue != "本尊\nsecond" || <-b.queue != "/exit" {
		t.Fatal("queue order or Unicode changed")
	}
}

func TestEventsStayWithinThread(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "events")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	b := &broker{state: snapshot{ThreadID: "owned", Turns: []turn{{Status: "inProgress"}}}, events: file, finished: make(chan turn, 1)}
	event := func(method, raw string) { b.event(wire{Method: method, Params: json.RawMessage(raw)}) }
	event("item/agentMessage/delta", `{"threadId":"other","delta":"wrong"}`)
	event("item/agentMessage/delta", `{"threadId":"owned","delta":"本尊"}`)
	event("turn/completed", `{"threadId":"other","turn":{"id":"bad","status":"completed"}}`)
	if len(b.finished) != 0 {
		t.Fatal("other thread completed this turn")
	}
	event("turn/completed", `{"threadId":"owned","turn":{"id":"good","status":"completed"}}`)
	result := <-b.finished
	if result.Answer != "本尊" || result.ID != "good" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestInvalidCLI(t *testing.T) {
	for _, args := range [][]string{{"-unknown"}, {"-p", "-1", "hi"}, {"-d", "x", "-p", "12", "hi"}, {"-p", "12", "-s", "-q"}, {"-p", "12", "-json"}, {"-p", "12", "hello", "-unknown"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
}
