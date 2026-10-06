//go:build windows

package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeServer answers the broker's JSON-RPC calls with answer(method) and records every call.
// After a successful turn/start it reports the turn as completed, like Codex does.
type fakeServer struct {
	r     *rpc
	calls chan wire
}

func newFakeServer(t *testing.T, b *broker, answer func(method string) (string, string)) *fakeServer {
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close(); writer.Close() })
	f := &fakeServer{r: &rpc{in: writer, pending: make(map[string]chan wire), dead: make(chan struct{})}, calls: make(chan wire, 16)}
	go func() {
		decoder := json.NewDecoder(reader)
		for {
			var w wire
			if decoder.Decode(&w) != nil {
				return
			}
			f.calls <- w
			result, failure := answer(w.Method)
			f.r.mu.Lock()
			ch := f.r.pending[string(w.ID)]
			f.r.mu.Unlock()
			if failure != "" {
				ch <- wire{Error: json.RawMessage(failure)}
				continue
			}
			ch <- wire{Result: json.RawMessage(result)}
			if w.Method == "turn/start" {
				go b.event(wire{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`)})
			}
		}
	}()
	return f
}

func (f *fakeServer) methods(n int) []string {
	var got []string
	for i := 0; i < n; i++ {
		select {
		case w := <-f.calls:
			got = append(got, w.Method)
		case <-time.After(2 * time.Second):
			return got
		}
	}
	return got
}

func newIdleBroker() (*broker, *int) {
	released := 0
	b := &broker{state: snapshot{ThreadID: "thread", Status: "idle", Turns: []turn{}}, queue: make(chan string, 4), finished: make(chan turn, 4), stop: make(chan struct{})}
	b.release = func() { released++ }
	return b, &released
}

// runOne sends one prompt and waits until the broker is idle again and has let go.
func runOne(t *testing.T, b *broker, prompt string) {
	t.Helper()
	b.queue <- prompt
	b.state.Queued = 1
	go b.run()
	t.Cleanup(b.shutdown)
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		status := b.state.Status
		b.mu.Unlock()
		if status == "released" || status == "failed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("broker stuck", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestQueuesIntoTheCodexAppWhenItHasTheConversation(t *testing.T) {
	b, released := newIdleBroker()
	f := newFakeServer(t, b, func(method string) (string, string) {
		if method == "thread/resume" {
			return "", `{"code":-32600,"message":"thread thread already has an active writer"}`
		}
		return `{}`, ""
	})
	b.connect = func() (*rpc, error) { return f.r, nil }
	runOne(t, b, "go")
	if got := f.methods(2); len(got) != 2 || got[0] != "thread/resume" || got[1] != "thread/queue/add" {
		t.Fatal(got)
	}
	if b.state.Turns[0].Status != "delegated" || b.state.Completed != 1 || *released == 0 {
		t.Fatal(b.state, *released)
	}
}

func TestQueuedPromptKeepsSkillAndThread(t *testing.T) {
	b, _ := newIdleBroker()
	var params struct {
		ThreadID, ClientUserMessageID string
		Input                         []map[string]any
	}
	f := newFakeServer(t, b, func(method string) (string, string) {
		if method == "thread/resume" {
			return "", `{"code":-32600,"message":"already has an active writer"}`
		}
		return `{}`, ""
	})
	b.connect = func() (*rpc, error) { return f.r, nil }
	b.inputs = func(prompt string) ([]any, error) {
		return []any{object{"type": "text", "text": prompt}, object{"type": "skill", "name": "tap", "path": "x"}}, nil
	}
	runOne(t, b, "/tap -done")
	for i := 0; i < 2; i++ {
		w := <-f.calls
		if w.Method == "thread/queue/add" {
			_ = json.Unmarshal(w.Params, &params)
		}
	}
	if params.ThreadID != "thread" || len(params.ClientUserMessageID) != 36 || len(params.Input) != 2 || params.Input[1]["type"] != "skill" {
		t.Fatal(params)
	}
}

func TestTakesTheConversationBackWhenItIsFree(t *testing.T) {
	b, released := newIdleBroker()
	f := newFakeServer(t, b, func(string) (string, string) { return `{}`, "" })
	connects := 0
	b.connect = func() (*rpc, error) { connects++; return f.r, nil }
	runOne(t, b, "go")
	if got := f.methods(2); len(got) != 2 || got[0] != "thread/resume" || got[1] != "turn/start" {
		t.Fatal(got)
	}
	if connects != 1 || b.state.Turns[0].Status != "completed" || *released == 0 {
		t.Fatal(connects, b.state, *released)
	}
}

func TestHolderStartsTurnsDirectlyAndLetsGoWhenIdle(t *testing.T) {
	b, released := newIdleBroker()
	f := newFakeServer(t, b, func(string) (string, string) { return `{}`, "" })
	b.r, b.holding = f.r, true
	runOne(t, b, "first")
	if got := f.methods(1); len(got) != 1 || got[0] != "turn/start" {
		t.Fatal(got)
	}
	if *released != 1 || b.holding || b.r != nil {
		t.Fatal(*released, b.holding)
	}
}

func TestClosingArchivesTheConversation(t *testing.T) {
	b, _ := newIdleBroker()
	f := newFakeServer(t, b, func(string) (string, string) { return `{}`, "" })
	connects := 0
	b.connect = func() (*rpc, error) { connects++; return f.r, nil }
	b.archive()
	if got := f.methods(1); len(got) != 1 || got[0] != "thread/archive" || connects != 1 {
		t.Fatal(got, connects)
	}
}

func TestClosingSurvivesARefusedArchive(t *testing.T) {
	b, _ := newIdleBroker()
	f := newFakeServer(t, b, func(string) (string, string) {
		return "", `{"code":-32600,"message":"thread thread already has an active writer"}`
	})
	b.r = f.r
	b.archive()
	if !strings.Contains(b.state.Error, "not archived") {
		t.Fatal(b.state.Error)
	}
}

func TestNewUUIDShape(t *testing.T) {
	a, b := newUUID(), newUUID()
	if len(a) != 36 || a[14] != '4' || a == b {
		t.Fatal(a, b)
	}
}
