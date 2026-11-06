//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCodex plays Codex around a hand-off: the thread's persistent queue, the turns that
// another writer (the Codex app) runs from it, and that writer's lock.
type fakeCodex struct {
	mu         sync.Mutex
	queued     []queuedPrompt
	turns      []map[string]any // newest last
	writer     bool             // another app-server holds the conversation
	takes      bool             // that writer runs queued prompts
	finishes   bool             // and lets them complete
	letsGo     bool             // and lets go of the conversation once they have
	answer     string
	added      []json.RawMessage
	archives   int
	archiveErr string
	readPath   string
	nextID     int
}

type queuedPrompt struct{ id, client string }

func (c *fakeCodex) writerActive() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writer, nil
}

// advance lets the writer start queued prompts and, one poll later, finish them.
func (c *fakeCodex) advance() {
	if !c.writer || !c.takes {
		return
	}
	for _, t := range c.turns {
		if t["completedAt"] == nil && c.finishes {
			t["completedAt"] = 1
			t["status"] = "completed"
			t["items"] = append(t["items"].([]map[string]any), map[string]any{"type": "agentMessage", "text": c.answer})
		}
	}
	for _, q := range c.queued {
		c.nextID++
		c.turns = append(c.turns, map[string]any{"id": fmt.Sprint("turn-", c.nextID), "status": "interrupted", "completedAt": nil, "error": nil,
			"items": []map[string]any{{"type": "userMessage", "clientId": q.client}}})
	}
	c.queued = nil
	if c.letsGo {
		idle := true
		for _, t := range c.turns {
			idle = idle && t["completedAt"] != nil
		}
		c.writer = !idle
	}
}

func (c *fakeCodex) handle(w wire) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch w.Method {
	case "thread/resume":
		if c.writer {
			return "", `{"code":-32600,"message":"thread thread already has an active writer"}`
		}
	case "thread/queue/add":
		var p struct{ ClientUserMessageID string }
		_ = json.Unmarshal(w.Params, &p)
		c.added = append(c.added, w.Params)
		c.nextID++
		id := fmt.Sprint("queued-", c.nextID)
		c.queued = append(c.queued, queuedPrompt{id, p.ClientUserMessageID})
		return `{"queuedSubmission":{"id":"` + id + `"}}`, ""
	case "thread/queue/list":
		c.advance()
		data := []map[string]string{}
		for _, q := range c.queued {
			data = append(data, map[string]string{"id": q.id})
		}
		raw, _ := json.Marshal(map[string]any{"data": data, "nextCursor": nil})
		return string(raw), ""
	case "thread/queue/delete":
		var p struct{ QueuedSubmissionID string }
		_ = json.Unmarshal(w.Params, &p)
		for i, q := range c.queued {
			if q.id == p.QueuedSubmissionID {
				c.queued = append(c.queued[:i], c.queued[i+1:]...)
				return `{}`, ""
			}
		}
		return "", `{"code":-32600,"message":"queued submission not found"}`
	case "thread/turns/list":
		c.advance()
		data := []map[string]any{}
		for i := len(c.turns) - 1; i >= 0; i-- {
			data = append(data, c.turns[i])
		}
		raw, _ := json.Marshal(map[string]any{"data": data})
		return string(raw), ""
	case "thread/archive":
		c.archives++
		if c.writer {
			return "", `{"code":-32600,"message":"thread thread already has an active writer"}`
		}
		if c.archiveErr != "" {
			return "", c.archiveErr
		}
	case "thread/read":
		raw, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "thread", "path": c.readPath}})
		return string(raw), ""
	}
	return `{}`, ""
}

func fastHandOff(t *testing.T) {
	grace, rewake, poll, archive, archiveWait := appGrace, rewakeEvery, pollEvery, archivePoll, archiveGrace
	appGrace, rewakeEvery, pollEvery, archivePoll, archiveGrace = 200*time.Millisecond, 50*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, time.Second
	t.Cleanup(func() {
		appGrace, rewakeEvery, pollEvery, archivePoll, archiveGrace = grace, rewake, poll, archive, archiveWait
	})
}

func newAppBroker(t *testing.T, c *fakeCodex) (*broker, *fakeServer) {
	fastHandOff(t)
	b, _ := newIdleBroker()
	f := newFakeServerFor(t, b, c.handle)
	b.connect = func() (*rpc, error) { return f.r, nil }
	b.writerActive = c.writerActive
	return b, f
}

func (f *fakeServer) seen() []string {
	var got []string
	for {
		select {
		case w := <-f.calls:
			got = append(got, w.Method)
		default:
			return got
		}
	}
}

func contains(methods []string, method string) bool {
	for _, m := range methods {
		if m == method {
			return true
		}
	}
	return false
}

func TestHandsPromptToTheCodexApp(t *testing.T) {
	c := &fakeCodex{takes: true, finishes: true, answer: "DONE"}
	b, f := newAppBroker(t, c)
	wakes := 0
	b.wake = func() error { c.mu.Lock(); defer c.mu.Unlock(); wakes++; c.writer = true; return nil }
	runOne(t, b, "go")
	methods := f.seen()
	if contains(methods, "thread/resume") || contains(methods, "turn/start") || !contains(methods, "thread/queue/add") {
		t.Fatal("the broker ran the turn instead of the app:", methods)
	}
	got := b.state.Turns[0]
	if wakes != 1 || got.Status != "completed" || got.Answer != "DONE" || got.ID == "" || b.state.Completed != 1 {
		t.Fatal(wakes, b.state)
	}
}

func TestPromptJoinsTheWriterWithoutWaking(t *testing.T) {
	c := &fakeCodex{writer: true, takes: true, finishes: true, answer: "LATER"}
	b, f := newAppBroker(t, c)
	b.wake = func() error { t.Error("woke the app although a writer holds the conversation"); return nil }
	runOne(t, b, "go")
	if got := b.state.Turns[0]; got.Status != "completed" || got.Answer != "LATER" || contains(f.seen(), "turn/start") {
		t.Fatal(b.state)
	}
}

func TestQueuesIntoTheWriterThatHoldsTheConversation(t *testing.T) {
	c := &fakeCodex{writer: true, takes: true, finishes: true, answer: "OK"}
	b, f := newAppBroker(t, c)
	b.inputs = func(prompt string) ([]any, error) {
		return []any{object{"type": "text", "text": prompt}, object{"type": "skill", "name": "tap", "path": "x"}}, nil
	}
	runOne(t, b, "/tap -done")
	methods := f.seen()
	if len(methods) < 2 || methods[0] != "thread/resume" || methods[1] != "thread/queue/add" || contains(methods, "turn/start") {
		t.Fatal(methods)
	}
	var params struct {
		ThreadID, ClientUserMessageID string
		Input                         []map[string]any
	}
	_ = json.Unmarshal(c.added[0], &params)
	if params.ThreadID != "thread" || len(params.ClientUserMessageID) != 36 || len(params.Input) != 2 || params.Input[1]["type"] != "skill" {
		t.Fatal(params)
	}
	if b.state.Turns[0].Status != "completed" || b.state.Turns[0].Answer != "OK" {
		t.Fatal(b.state)
	}
}

func TestRunsPromptHereWhenTheAppNeverTakesIt(t *testing.T) {
	c := &fakeCodex{}
	b, f := newAppBroker(t, c)
	wakes := 0
	b.wake = func() error { c.mu.Lock(); defer c.mu.Unlock(); wakes++; return nil }
	runOne(t, b, "go")
	methods := f.seen()
	if !contains(methods, "thread/queue/delete") || !contains(methods, "thread/resume") || !contains(methods, "turn/start") {
		t.Fatal(methods)
	}
	if wakes < 2 || b.state.Turns[0].Status != "completed" || len(c.queued) != 0 {
		t.Fatal(wakes, b.state, c.queued)
	}
}

func TestRunsPromptHereWhenTheAppCannotBeWoken(t *testing.T) {
	c := &fakeCodex{}
	b, f := newAppBroker(t, c)
	appGrace = time.Hour
	wakes := 0
	b.wake = func() error { wakes++; return errors.New("no app") }
	runOne(t, b, "go")
	if methods := f.seen(); wakes != 1 || !contains(methods, "turn/start") || b.state.Turns[0].Status != "completed" {
		t.Fatal(wakes, methods, b.state)
	}
}

// waitClosed waits until a broker asked to quit has drained its work.
func waitClosed(t *testing.T, b *broker) {
	t.Helper()
	select {
	case <-b.stop:
	case <-time.After(3 * time.Second):
		b.mu.Lock()
		defer b.mu.Unlock()
		t.Fatal("broker did not close", b.state)
	}
}

func TestFinalPromptRunsHereSoItCanBeArchived(t *testing.T) {
	c := &fakeCodex{takes: true, finishes: true}
	b, f := newAppBroker(t, c)
	b.wake = func() error { t.Error("woke the app for the prompt sent with -q"); return nil }
	if postPrompt(b, "/prompt-and-exit", "update today's weather") != 200 {
		t.Fatal("prompt with -q rejected")
	}
	go b.run()
	waitClosed(t, b)
	methods := f.seen()
	if contains(methods, "thread/queue/add") || !contains(methods, "thread/resume") || !contains(methods, "turn/start") {
		t.Fatal("the prompt sent with -q did not run in the broker:", methods)
	}
	if got := b.state.Turns[0]; got.Status != "completed" || b.state.Status != "closed" {
		t.Fatal(b.state)
	}
}

func TestFinalPromptJoinsTheAppThatHoldsTheConversation(t *testing.T) {
	c := &fakeCodex{writer: true, takes: true, finishes: true, answer: "LAST"}
	b, f := newAppBroker(t, c)
	b.wake = func() error { t.Error("woke the app although it holds the conversation"); return nil }
	if postPrompt(b, "/prompt-and-exit", "last") != 200 {
		t.Fatal("prompt with -q rejected")
	}
	go b.run()
	waitClosed(t, b)
	if methods := f.seen(); contains(methods, "turn/start") || !contains(methods, "thread/queue/add") {
		t.Fatal(methods)
	}
	if got := b.state.Turns[0]; got.Status != "completed" || got.Answer != "LAST" {
		t.Fatal(b.state)
	}
}

func TestOnlyThePromptSentWithQuitRunsHere(t *testing.T) {
	c := &fakeCodex{takes: true, finishes: true, letsGo: true, answer: "APP"}
	b, f := newAppBroker(t, c)
	wakes := 0
	b.wake = func() error { c.mu.Lock(); defer c.mu.Unlock(); wakes++; c.writer = true; return nil }
	if postPrompt(b, "/prompt", "first") != 200 || b.finalAt != 0 {
		t.Fatal("a prompt without -q was marked final")
	}
	if postPrompt(b, "/prompt-and-exit", "last") != 200 || b.finalAt != 2 {
		t.Fatal("the prompt sent with -q was not marked final", b.finalAt)
	}
	go b.run()
	waitClosed(t, b)
	if wakes != 1 || len(c.added) != 1 || !contains(f.seen(), "turn/start") {
		t.Fatal(wakes, len(c.added))
	}
	if first, last := b.state.Turns[0], b.state.Turns[1]; first.Answer != "APP" || last.Prompt != "last" || last.Status != "completed" {
		t.Fatal(b.state.Turns)
	}
}

func TestStandaloneQuitLeavesQueuedPromptsToTheApp(t *testing.T) {
	b, _ := newIdleBroker()
	if postPrompt(b, "/prompt", "work") != 200 || postPrompt(b, "/prompt", "/exit") != 200 || b.finalAt != 0 || !b.closing {
		t.Fatal(b.finalAt, b.closing)
	}
}

func TestTurnEndsWhenItsWriterLeaves(t *testing.T) {
	c := &fakeCodex{writer: true, takes: true}
	b, _ := newAppBroker(t, c)
	b.wake = func() error { return nil }
	go func() {
		for {
			c.mu.Lock()
			started := len(c.turns) > 0
			if started {
				c.writer = false
			}
			c.mu.Unlock()
			if started {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	runOne(t, b, "go")
	if got := b.state.Turns[0]; got.Status != "interrupted" || got.Error == "" {
		t.Fatal(b.state)
	}
}

func TestArchiveWaitsForTheAppToLetGo(t *testing.T) {
	c := &fakeCodex{writer: true}
	b, _ := newAppBroker(t, c)
	checks := 0
	b.writerActive = func() (bool, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if checks++; checks == 5 {
			c.writer = false
		}
		return c.writer, nil
	}
	b.archiveWithRetry()
	if !b.state.Archived || b.state.Error != "" || c.archives != 1 || b.state.Status != "archiving" {
		t.Fatal(b.state, c.archives)
	}
}

func TestArchiveLeavesTheConversationToTheAppThatKeepsIt(t *testing.T) {
	c := &fakeCodex{writer: true}
	b, _ := newAppBroker(t, c)
	archiveGrace = 30 * time.Millisecond
	start := time.Now()
	b.archiveWithRetry()
	if b.state.Archived || !b.state.LeftOpen || b.state.Error != "" || c.archives != 0 {
		t.Fatal(b.state, c.archives)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waited too long for an app that keeps the conversation")
	}
}

func TestArchiveKeepsWaitingAfterAWriterRefusal(t *testing.T) {
	c := &fakeCodex{}
	b, _ := newAppBroker(t, c)
	b.writerActive = func() (bool, error) { return false, nil } // the lock looks free, Codex disagrees
	c.writer = true
	go func() { time.Sleep(50 * time.Millisecond); c.mu.Lock(); c.writer = false; c.mu.Unlock() }()
	b.archiveWithRetry()
	if !b.state.Archived || c.archives < 2 {
		t.Fatal(b.state, c.archives)
	}
}

func TestArchiveAcceptsArchivalInTheApp(t *testing.T) {
	c := &fakeCodex{archiveErr: `{"code":-32600,"message":"no rollout found for thread id thread"}`, readPath: `\\?\C:\Users\u\.codex\archived_sessions\rollout-thread.jsonl`}
	b, _ := newAppBroker(t, c)
	b.archiveWithRetry()
	if !b.state.Archived || b.state.Error != "" {
		t.Fatal(b.state)
	}
	c = &fakeCodex{archiveErr: c.archiveErr, readPath: `C:\Users\u\.codex\sessions\rollout-thread.jsonl`}
	b, _ = newAppBroker(t, c)
	b.archiveWithRetry()
	if b.state.Archived || !strings.Contains(b.state.Error, "not archived") || c.archives != 3 {
		t.Fatal(b.state, c.archives)
	}
}

func TestArchiveLetsTheAppRunPromptsStillQueued(t *testing.T) {
	c := &fakeCodex{takes: true, finishes: true, queued: []queuedPrompt{{"queued-0", "phone"}}}
	b, _ := newAppBroker(t, c)
	b.wake = func() error { c.mu.Lock(); defer c.mu.Unlock(); c.writer = true; return nil }
	go func() {
		for {
			c.mu.Lock()
			c.advance() // the app works through its queue on its own
			done := len(c.turns) > 0 && c.turns[0]["completedAt"] != nil
			if done {
				c.writer = false // the app lets go of the idle conversation
			}
			c.mu.Unlock()
			if done {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	b.archiveWithRetry()
	if !b.state.Archived || len(c.turns) != 1 {
		t.Fatal(b.state, c.turns)
	}
}

func TestCancelRefusesTurnsRunningInTheApp(t *testing.T) {
	b := &broker{r: &rpc{}, state: snapshot{ThreadID: "thread", Turns: []turn{{ID: "turn", Status: "inProgress"}}}}
	req := httptest.NewRequest("POST", "/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	b.handler(endpoint{Token: "secret"})(response, req)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "Codex app") {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestWakeLinkRejectsInvalidThreads(t *testing.T) {
	for _, id := range []string{"", "thread", "../../x", "01a1ac42-6b2f-7f31-ad36-482a17e3475z"} {
		if openWakeLink(id) == nil {
			t.Fatal("accepted", id)
		}
	}
}
