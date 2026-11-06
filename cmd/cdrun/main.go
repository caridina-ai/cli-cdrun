//go:build windows

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

type object = map[string]any
type endpoint struct {
	PID                   int
	URL, Token, Dir, Name string
	Created               uint64
	Executable            string
	FinalPath             string
	ThreadID              string
	Native                bool
}
type request struct{ Prompt string }
type turn struct {
	Prompt, ID, Status, Answer, Error string
	Sequence                          int
	Truncated                         bool
}
type snapshot struct {
	PID                                               int
	ThreadID, Cwd, Name, Model, Status, Error, LogDir string
	Completed, Queued                                 int
	FirstSequence                                     int
	Turns                                             []turn
	Archived                                          bool
	LeftOpen                                          bool // another writer, usually the Codex app, still holds the conversation
	Native                                            bool
}
type wire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}
type rpc struct {
	in          io.WriteCloser
	mu, writeMu sync.Mutex
	next        int
	pending     map[string]chan wire
	dead        chan struct{}
	notify      func(wire)
}

func endpointPath(pid int) string { return filepath.Join(stateRoot(), strconv.Itoa(pid)+".json") }
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func (r *rpc) send(v any) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return json.NewEncoder(r.in).Encode(v)
}
func (r *rpc) call(method string, params any) (json.RawMessage, error) {
	r.mu.Lock()
	r.next++
	id := r.next
	key := strconv.Itoa(id)
	ch := make(chan wire, 1)
	r.pending[key] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, key); r.mu.Unlock() }()
	if err := r.send(object{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case v := <-ch:
		if len(v.Error) > 0 && string(v.Error) != "null" {
			return nil, fmt.Errorf("%s: %s", method, v.Error)
		}
		return v.Result, nil
	case <-r.dead:
		return nil, errors.New("Codex app-server disconnected")
	case <-time.After(60 * time.Second):
		return nil, fmt.Errorf("%s timed out; result is unknown", method)
	}
}
func (r *rpc) read(out io.Reader) {
	defer close(r.dead)
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 65536), 16*1024*1024)
	for sc.Scan() {
		var w wire
		if json.Unmarshal(sc.Bytes(), &w) != nil {
			continue
		}
		if len(w.ID) > 0 && w.Method == "" {
			r.mu.Lock()
			ch := r.pending[string(w.ID)]
			r.mu.Unlock()
			if ch != nil {
				select {
				case ch <- w:
				default:
				}
			}
			continue
		}
		if w.Method != "" {
			r.notify(w)
		}
		if len(w.ID) > 0 && w.Method != "" {
			// Never silently grant server-initiated approval requests.
			_ = r.send(object{"id": w.ID, "error": object{"code": -32601, "message": "cdrun has no interactive approval UI"}})
		}
	}
}

// A broker owns one Codex conversation. Codex lets a single app-server write to a conversation
// at a time, and the Codex app's windows and paired phones all talk to the app's own app-server.
// So the broker hands each prompt to the Codex app (see app.go): the app runs the turn, every
// client of the app can watch and steer it, and the broker follows it from a private app-server
// that only reads. Without the app, the broker's private app-server runs the turn itself and is
// stopped once idle, leaving the conversation free.
type broker struct {
	mu           sync.Mutex
	state        snapshot
	r            *rpc
	queue        chan string
	finished     chan turn
	stop         chan struct{}
	once         sync.Once
	events       *os.File
	closing      bool
	inputs       func(string) ([]any, error)
	connect      func() (*rpc, error) // starts a fresh app-server; nil keeps r as it is
	release      func()               // stops it again, freeing the conversation
	holding      bool                 // this broker's app-server is the conversation's writer
	wake         func() error         // asks the Codex app to run queued prompts; nil runs them here
	writerActive func() (bool, error) // another app-server is the conversation's writer
	accepted     int                  // queue entries accepted so far
	taken        int                  // queue entries the broker has taken up so far
	finalAt      int                  // position of the prompt sent with -q; 0 when none was
}

func (b *broker) shutdown() { b.once.Do(func() { close(b.stop) }) }
func (b *broker) event(w wire) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Delta    string `json:"delta"`
		Turn     struct {
			ID, Status string
			Error      json.RawMessage
		} `json:"turn"`
		Item struct{ Type, Text string } `json:"item"`
	}
	_ = json.Unmarshal(w.Params, &p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.ThreadID != "" && p.ThreadID != b.state.ThreadID {
		return
	}
	switch w.Method {
	case "turn/started", "turn/completed", "item/agentMessage/delta", "item/completed", "error":
		if b.events != nil {
			_ = appendEvent(b.events, object{"time": time.Now().Format(time.RFC3339Nano), "method": w.Method, "params": w.Params})
		}
	}
	if len(w.ID) > 0 {
		b.state.Error = "Unsupported server request: " + w.Method
	}
	if len(b.state.Turns) == 0 {
		return
	}
	t := &b.state.Turns[len(b.state.Turns)-1]
	if t.Status != "inProgress" {
		return
	}
	eventTurn := p.TurnID
	if p.Turn.ID != "" {
		eventTurn = p.Turn.ID
	}
	if t.ID != "" && eventTurn != "" && eventTurn != t.ID {
		return
	}
	switch w.Method {
	case "turn/started":
		t.ID = p.Turn.ID
	case "item/agentMessage/delta":
		appendAnswer(t, p.Delta)
	case "item/completed":
		if p.Item.Type == "agentMessage" && p.Item.Text != "" && !strings.HasSuffix(t.Answer, p.Item.Text) {
			appendAnswer(t, p.Item.Text+"\n")
		}
	case "turn/completed":
		t.ID = p.Turn.ID
		t.Status = p.Turn.Status
		if len(p.Turn.Error) > 0 && string(p.Turn.Error) != "null" {
			t.Error = string(p.Turn.Error)
		}
		select {
		case b.finished <- *t:
		default:
		}
	}
}
func (b *broker) run() {
	for {
		select {
		case <-b.stop:
			return
		case prompt := <-b.queue:
			b.mu.Lock()
			b.state.Queued--
			b.taken++
			final := b.finalAt != 0 && b.taken == b.finalAt
			if prompt == "/exit" {
				b.state.Status = "closed"
				b.mu.Unlock()
				b.shutdown()
				return
			}
			b.state.Status = "running"
			b.state.Turns = append(b.state.Turns, turn{Prompt: prompt, Status: "inProgress", Sequence: b.state.Completed + 1})
			if len(b.state.Turns) > maxTurns {
				b.state.Turns = append([]turn(nil), b.state.Turns[len(b.state.Turns)-maxTurns:]...)
			}
			b.state.FirstSequence = b.state.Turns[0].Sequence
			b.mu.Unlock()
			if !b.deliver(prompt, final) {
				return
			}
			b.releaseWhenIdle()
			b.mu.Lock()
			if b.closing && len(b.queue) == 0 {
				b.state.Status = "closed"
				b.mu.Unlock()
				b.shutdown()
				return
			}
			b.mu.Unlock()
		}
	}
}

// deliver runs one prompt to its end: in the Codex app when it is available (see handOff),
// otherwise in this broker's app-server, or in the queue of whichever app-server has the
// conversation open. It returns false after a fatal failure; queued work is then dropped,
// never replayed.
//
// The prompt sent with -q (final) runs in this broker's app-server whenever the conversation
// is free. The Codex app keeps every conversation it ran open until it quits, and Codex cannot
// archive a conversation another app-server holds, so only a turn the broker runs itself can
// be archived as soon as it ends. Such a one-off or last task is meant to finish and close,
// not to be followed elsewhere; the app's windows and phones see the conversation as in use
// meanwhile. When the app already holds the conversation, the prompt joins the app's queue.
func (b *broker) deliver(prompt string, final bool) bool {
	input, err := b.prepare(prompt)
	if err != nil {
		b.finishTurn("failed", err.Error())
		return true
	}
	if b.holding {
		return b.startTurn(input, newUUID())
	}
	if b.wake == nil || final {
		held, err := b.resume()
		if err != nil {
			return b.fatal(err)
		}
		if held {
			return b.startTurn(input, newUUID())
		}
	}
	return b.handOff(input, final)
}

// startTurn runs a turn in this broker's app-server, which must hold the conversation.
func (b *broker) startTurn(input []any, client string) bool {
	r := b.r
	if _, err := r.call("turn/start", object{"threadId": b.state.ThreadID, "clientUserMessageId": client, "input": input}); err != nil {
		return b.fatal(err)
	}
	select {
	case result := <-b.finished:
		b.mu.Lock()
		b.state.Completed++
		b.state.Status = result.Status
		if result.Status == "completed" {
			b.state.Status = "idle"
		}
		b.mu.Unlock()
		return true
	case <-r.dead:
		b.mu.Lock()
		b.state.Status = "failed"
		b.state.Error = "Codex app-server disconnected"
		b.mu.Unlock()
		b.letGo()
		return false
	case <-b.stop:
		return false
	}
}

// prepare makes sure an app-server is running and turns the prompt into Codex input.
func (b *broker) prepare(prompt string) ([]any, error) {
	if err := b.ensureClient(); err != nil {
		return nil, err
	}
	return b.input(prompt)
}

func (b *broker) input(prompt string) ([]any, error) {
	if b.inputs == nil {
		return []any{object{"type": "text", "text": prompt}}, nil
	}
	return b.inputs(prompt)
}

// resume tries to become the conversation's writer again. It reports false, without error,
// when another app-server (the Codex app) has the conversation open.
func (b *broker) resume() (bool, error) {
	_, err := b.r.call("thread/resume", object{"threadId": b.state.ThreadID, "excludeTurns": true})
	if err != nil {
		if strings.Contains(err.Error(), "active writer") {
			return false, nil
		}
		return false, err
	}
	b.mu.Lock()
	b.holding = true
	b.mu.Unlock()
	return true, nil
}

func (b *broker) finishTurn(status, failure string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := &b.state.Turns[len(b.state.Turns)-1]
	t.Status, t.Error = status, failure
	b.state.Completed++
	b.state.Status = "idle"
}

func (b *broker) fatal(err error) bool {
	b.mu.Lock()
	b.state.Status = "failed"
	b.state.Error = err.Error()
	b.state.Turns[len(b.state.Turns)-1].Status = "failed"
	b.state.Turns[len(b.state.Turns)-1].Error = err.Error()
	b.state.Completed++
	b.state.Queued = 0
	if b.closing {
		b.shutdown()
	}
	b.mu.Unlock()
	b.letGo()
	return false
}

// releaseWhenIdle frees the conversation once nothing is waiting, so the Codex app can open it.
func (b *broker) releaseWhenIdle() {
	if len(b.queue) > 0 || b.release == nil {
		return
	}
	b.letGo()
	b.mu.Lock()
	if b.state.Status != "failed" && b.state.Status != "closed" {
		b.state.Status = "released"
	}
	b.mu.Unlock()
}

// archive asks Codex once to archive the conversation. It records success only after Codex
// confirms it, or shows that the conversation was already archived elsewhere, such as in the
// Codex app. It reports whether another app-server, as the conversation's writer, refused.
func (b *broker) archive() (refusedByWriter bool) {
	if err := b.ensureClient(); err != nil {
		b.mu.Lock()
		b.state.Error = "conversation not archived: " + err.Error()
		b.mu.Unlock()
		return false
	}
	if _, err := b.r.call("thread/archive", object{"threadId": b.state.ThreadID}); err != nil {
		if !strings.Contains(err.Error(), "no rollout found") || !b.archivedElsewhere() {
			b.mu.Lock()
			b.state.Error = "conversation not archived: " + err.Error()
			b.mu.Unlock()
			return strings.Contains(err.Error(), "active writer")
		}
	}
	b.mu.Lock()
	b.state.Archived = true
	if strings.HasPrefix(b.state.Error, "conversation not archived:") {
		b.state.Error = ""
	}
	b.mu.Unlock()
	return false
}

// archivedElsewhere recognizes a conversation that Codex already moved to archived_sessions.
func (b *broker) archivedElsewhere() bool {
	raw, err := b.r.call("thread/read", object{"threadId": b.state.ThreadID})
	if err != nil {
		return false
	}
	var read struct{ Thread struct{ ID, Path string } }
	if json.Unmarshal(raw, &read) != nil || read.Thread.ID != b.state.ThreadID {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(read.Thread.Path), "/") {
		if strings.EqualFold(part, "archived_sessions") {
			return true
		}
	}
	return false
}

// archivePoll paces the wait for another writer to let go of the conversation; archiveGrace
// bounds that wait.
var archivePoll, archiveGrace = 5 * time.Second, 15 * time.Second

// archiveWithRetry archives the conversation once all accepted work has finished. Codex refuses
// to archive a conversation that another app-server holds, and nothing can ask that writer to
// let go. Usually it is the Codex app, which keeps a conversation it ran open until it quits.
// So when a writer still holds the conversation after a short grace, the broker ends anyway and
// leaves the conversation open there (LeftOpen), to be archived in the app. No work is cut
// short: the app's own turns go on. Other failures get three attempts.
func (b *broker) archiveWithRetry() {
	b.mu.Lock()
	b.state.Status = "archiving"
	b.mu.Unlock()
	deadline := time.Now().Add(archiveGrace)
	failures := 0
	for {
		// Release our writer before each attempt; a new server archives persisted state.
		b.letGo()
		held := b.othersWriting()
		if !held && b.wake != nil && b.ensureClient() == nil {
			// Prompts still waiting in Codex's queue, from the app or a phone, run in the app.
			if waiting, err := b.queuedInCodex(); err == nil && waiting > 0 && b.wake() == nil {
				held = true
			}
		}
		if !held {
			if held = b.archive(); b.state.Archived {
				return
			}
			if !held {
				if failures++; failures == 3 {
					return
				}
				time.Sleep(time.Second)
				continue
			}
		}
		if time.Now().After(deadline) {
			b.mu.Lock()
			b.state.LeftOpen = true
			if strings.HasPrefix(b.state.Error, "conversation not archived:") {
				b.state.Error = ""
			}
			b.mu.Unlock()
			return
		}
		time.Sleep(archivePoll)
	}
}

// othersWriting reports whether an app-server other than this broker's holds the conversation.
func (b *broker) othersWriting() bool {
	if b.writerActive == nil || b.holding {
		return false
	}
	active, err := b.writerActive()
	return err == nil && active
}

// ensureClient starts a private app-server when the broker has none.
func (b *broker) ensureClient() error {
	if b.r != nil {
		select {
		case <-b.r.dead:
		default:
			return nil
		}
	}
	if b.connect == nil {
		return errors.New("Codex app-server disconnected")
	}
	r, err := b.connect()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.r, b.holding = r, false
	b.mu.Unlock()
	return nil
}

func (b *broker) letGo() {
	if b.release == nil {
		return
	}
	b.mu.Lock()
	b.r, b.holding = nil, false
	b.mu.Unlock()
	b.release()
}

func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	h := hex.EncodeToString(u[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func normalize(s string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"))
	if !utf8.ValidString(s) {
		return "", errors.New("prompt must be UTF-8")
	}
	if utf8.RuneCountInString(s) > 8000 {
		return "", errors.New("prompt exceeds 8000 characters")
	}
	if strings.ContainsRune(s, 0) {
		return "", errors.New("prompt contains NUL")
	}
	return s, nil
}
func (b *broker) handler(ep endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+ep.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == "GET" && r.URL.Path == "/status":
			b.mu.Lock()
			defer b.mu.Unlock()
			_ = json.NewEncoder(w).Encode(b.state)
		case r.Method == "POST" && (r.URL.Path == "/prompt" || r.URL.Path == "/prompt-and-exit"):
			var q request
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF {
				http.Error(w, "invalid request", 400)
				return
			}
			prompt, err := normalize(q.Prompt)
			if err != nil || prompt == "" {
				http.Error(w, "invalid prompt", 400)
				return
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if prompt == "/exit" && b.closing {
				_ = json.NewEncoder(w).Encode(object{"closing": true})
				return
			}
			if prompt == "/exit" && b.state.Status == "failed" {
				b.shutdown()
				_ = json.NewEncoder(w).Encode(object{"closing": true})
				return
			}
			if b.closing || b.state.Status == "closed" || (b.state.Error != "" && prompt != "/exit") {
				http.Error(w, "session unavailable: "+b.state.Error, 409)
				return
			}
			if prompt == "/exit" && len(b.queue) == cap(b.queue) {
				b.closing = true
				_ = json.NewEncoder(w).Encode(object{"closing": true})
				return
			}
			select {
			case b.queue <- prompt:
				b.state.Queued++
				b.accepted++
				if r.URL.Path == "/prompt-and-exit" {
					b.finalAt = b.accepted
				}
				if prompt == "/exit" || r.URL.Path == "/prompt-and-exit" {
					b.closing = true
				}
				_ = json.NewEncoder(w).Encode(object{"accepted": true})
			default:
				http.Error(w, "queue full", 429)
			}
		case r.Method == "POST" && r.URL.Path == "/cancel":
			b.mu.Lock()
			var id string
			if n := len(b.state.Turns); n > 0 && b.state.Turns[n-1].Status == "inProgress" {
				id = b.state.Turns[n-1].ID
			}
			thread := b.state.ThreadID
			r, holding := b.r, b.holding
			b.mu.Unlock()
			if id != "" && !holding {
				http.Error(w, "this turn runs in the Codex app; stop it there", 409)
				return
			}
			if id == "" || r == nil {
				http.Error(w, "no active turn yet", 409)
				return
			}
			if _, err := r.call("turn/interrupt", object{"threadId": thread, "turnId": id}); err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			_ = json.NewEncoder(w).Encode(object{"interrupted": id})
		case r.Method == "POST" && r.URL.Path == "/force-close":
			_ = json.NewEncoder(w).Encode(object{"closing": true})
			b.shutdown()
		default:
			http.Error(w, "not found", 404)
		}
	}
}

func ownJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, windows.CurrentProcess())
	}
	if err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}
func cleanEnv() []string {
	var out []string
	for _, v := range os.Environ() {
		name, _, _ := strings.Cut(v, "=")
		switch strings.ToUpper(name) {
		case "CODEX_APP_TOOLS_PIPE_PATH", "CODEX_SESSION_ID", "CODEX_THREAD_ID", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "CODEX_PERMISSION_PROFILE":
			continue
		}
		out = append(out, v)
	}
	return out
}
func resolveCodex() (string, error) {
	if explicit := os.Getenv("CDRUN_CODEX_EXE"); explicit != "" {
		info, err := os.Stat(explicit)
		if err != nil || !filepath.IsAbs(explicit) || info.IsDir() || !strings.EqualFold(filepath.Ext(explicit), ".exe") {
			return "", fmt.Errorf("CDRUN_CODEX_EXE must point to an existing absolute .exe path: %s", explicit)
		}
		return explicit, nil
	}
	if path, err := exec.LookPath("codex.exe"); err == nil {
		return path, nil
	}
	root := filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin")
	candidates, err := filepath.Glob(filepath.Join(root, "*", "codex.exe"))
	if err != nil {
		return "", err
	}
	var selected string
	var newest time.Time
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() && (selected == "" || info.ModTime().After(newest)) {
			selected = path
			newest = info.ModTime()
		}
	}
	if selected != "" {
		return selected, nil
	}
	legacy := filepath.Join(root, "codex.exe")
	if info, err := os.Stat(legacy); err == nil && !info.IsDir() {
		return legacy, nil
	}
	return "", errors.New("Codex executable not found on PATH or in the desktop installation; set CDRUN_CODEX_EXE to its absolute path")
}

// appServer is the session's private Codex app-server. The broker stops it whenever it is idle,
// which frees the conversation for the Codex app, and starts a fresh one for the next prompt.
type appServer struct {
	mu       sync.Mutex
	exe, dir string
	trust    bool
	stderr   io.Writer
	cmd      *exec.Cmd
	in       io.WriteCloser
}

func initializeRPC(r *rpc) error {
	if _, err := r.call("initialize", object{"clientInfo": object{"name": "cdrun", "title": "cdrun", "version": version}, "capabilities": object{"experimentalApi": true}}); err != nil {
		return err
	}
	return r.send(object{"method": "initialized", "params": object{}})
}

func (s *appServer) start(notify func(wire)) (*rpc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	args := []string{"app-server", "--stdio"}
	if s.trust {
		key, _ := json.Marshal(s.dir)
		args = append(args, "-c", "projects."+string(key)+".trust_level=\"trusted\"")
	}
	cmd := exec.Command(s.exe, args...)
	cmd.Dir = s.dir
	cmd.Env = cleanEnv()
	cmd.Stderr = s.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	s.cmd, s.in = cmd, input
	r := &rpc{in: input, pending: make(map[string]chan wire), dead: make(chan struct{}), notify: notify}
	go r.read(output)
	// The queue methods that reach the Codex app's copy of the conversation are experimental.
	if err = initializeRPC(r); err != nil {
		s.stopLocked()
		return nil, err
	}
	return r, nil
}

func (s *appServer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

// stopLocked closes stdin and waits briefly; an idle app-server exits on its own.
func (s *appServer) stopLocked() {
	if s.cmd == nil {
		return
	}
	s.in.Close()
	done := make(chan struct{})
	go func(c *exec.Cmd) { _ = c.Wait(); close(done) }(s.cmd)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd, s.in = nil, nil
}

func serve(dir, name, codexExe string, trust bool) error {
	if err := secureStateRoot(); err != nil {
		return err
	}
	job, err := ownJob()
	if err != nil {
		return fmt.Errorf("create session job: %w", err)
	}
	// Keep the handle alive until process exit; closing it also terminates this broker.
	brokerJob = job
	sessionDir := filepath.Join(stateRoot(), fmt.Sprintf("session-%d-%d", os.Getpid(), time.Now().Unix()))
	if err = os.MkdirAll(sessionDir, 0700); err != nil {
		return err
	}
	logs, err := os.Create(filepath.Join(sessionDir, "server-stderr.log"))
	if err != nil {
		return err
	}
	defer logs.Close()
	events, err := os.Create(filepath.Join(sessionDir, "events.jsonl"))
	if err != nil {
		return err
	}
	defer events.Close()
	server := &appServer{exe: codexExe, dir: dir, trust: trust, stderr: &cappedWriter{file: logs, limit: maxLogBytes}}
	defer server.stop()
	b := &broker{state: snapshot{PID: os.Getpid(), Cwd: dir, Name: name, Status: "idle", LogDir: sessionDir, Turns: []turn{}}, queue: make(chan string, 32), finished: make(chan turn, 16), stop: make(chan struct{}), events: events}
	b.connect = func() (*rpc, error) { return server.start(b.event) }
	b.release = server.stop
	if b.r, err = b.connect(); err != nil {
		return err
	}
	// Model, sandbox and approvals all come from the user's own Codex config, as in the Codex app.
	raw, err := b.r.call("thread/start", object{"cwd": dir})
	if err != nil {
		return err
	}
	var started struct {
		Thread struct{ ID string }
		Model  string
	}
	if err = json.Unmarshal(raw, &started); err != nil {
		return err
	}
	if started.Thread.ID == "" {
		return errors.New("missing thread id")
	}
	b.mu.Lock()
	b.state.ThreadID = started.Thread.ID
	b.state.Model = started.Model
	b.holding = true
	b.mu.Unlock()
	b.writerActive = func() (bool, error) { return nativeWriterActive(started.Thread.ID) }
	if os.Getenv("CDRUN_CODEX_APP") != "0" {
		b.wake = func() error { return wakeCodexApp(started.Thread.ID) }
	}
	b.inputs = func(prompt string) ([]any, error) { return skillInput(b.r, dir, prompt) }
	if _, err = b.r.call("thread/name/set", object{"threadId": started.Thread.ID, "name": name}); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(sessionDir, "session.json"), b.state); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		listener.Close()
		return err
	}
	ep := endpoint{PID: os.Getpid(), URL: "http://" + listener.Addr().String(), Token: hex.EncodeToString(token), Dir: dir, Name: name, FinalPath: filepath.Join(sessionDir, "final.json")}
	ep.Created, err = processBirth(windows.CurrentProcess())
	if err != nil {
		listener.Close()
		return err
	}
	ep.Executable, err = os.Executable()
	if err != nil {
		listener.Close()
		return err
	}
	httpServer := &http.Server{Handler: b.handler(ep), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	b.releaseWhenIdle()
	runDone := make(chan struct{})
	go func() { defer close(runDone); b.run() }()
	go func() {
		if e := httpServer.Serve(listener); e != nil && e != http.ErrServerClosed {
			b.shutdown()
		}
	}()
	if err = writeJSON(endpointPath(ep.PID), ep); err != nil {
		httpServer.Close()
		return err
	}
	defer os.Remove(endpointPath(ep.PID))
	// The app-server comes and goes with the work; only an explicit close ends the session.
	<-b.stop
	<-runDone
	b.archiveWithRetry()
	httpServer.Close()
	b.mu.Lock()
	b.state.Status = "closed"
	err = writeJSON(ep.FinalPath, b.state)
	b.mu.Unlock()
	return err
}

func loadEndpoint(pid int) (endpoint, error) {
	var e endpoint
	b, err := os.ReadFile(endpointPath(pid))
	if err != nil {
		return e, fmt.Errorf("PID %d is not an active cdrun session", pid)
	}
	err = json.Unmarshal(b, &e)
	if err == nil {
		err = validateEndpoint(e, pid)
	}
	return e, err
}
func fetch(e endpoint, path string, prompt *string) ([]byte, error) {
	method := "GET"
	var body io.Reader
	if prompt != nil {
		method = "POST"
		data, _ := json.Marshal(request{Prompt: *prompt})
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if e.Native {
		client.Timeout = 4 * time.Minute
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("session unavailable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}
func launch(dir, name, prompt string, trust, quit bool) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(abs, 0755); err != nil {
		return err
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	codexExe, err := resolveCodex()
	if err != nil {
		return err
	}
	if err = secureStateRoot(); err != nil {
		return err
	}
	cleanStaleEndpoints()
	return launchProcess([]string{"__serve", abs, name, codexExe, strconv.FormatBool(trust)}, prompt, true, quit)
}

func launchProcess(args []string, prompt string, printPID, quit bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(stateRoot(), "launch-*.log")
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Env = cleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB}
	if err = cmd.Start(); err != nil {
		cmd.SysProcAttr.CreationFlags = windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP
		if err = cmd.Start(); err != nil {
			return err
		}
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		e, loadErr := loadEndpoint(pid)
		if loadErr == nil {
			if _, err = fetch(e, "/status", nil); err == nil {
				if prompt != "" || quit {
					if err = submitPrompt(e, prompt, quit); err != nil {
						empty := ""
						_, _ = fetch(e, "/force-close", &empty)
						return err
					}
				}
				if printPID {
					fmt.Println(pid)
				}
				return nil
			}
		}
		select {
		case <-exited:
			details, _ := os.ReadFile(f.Name())
			return fmt.Errorf("session startup failed: %s (log: %s)", strings.TrimSpace(string(details)), f.Name())
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	details, _ := os.ReadFile(f.Name())
	return fmt.Errorf("session startup failed: %s (log: %s)", strings.TrimSpace(string(details)), f.Name())
}

const version = "0.4.0"
const usage = `cdrun - start and drive Codex sessions by PID

usage:
  cdrun -d <working-dir> [-n <name>] [prompt] [-q]     start a session, or prompt the one
                                                       already running; prints its PID
  cdrun -n <name> <prompt> [-q]                        inject a prompt into that session
  cdrun -n <name> - [-q]                               inject a prompt read from stdin
  cdrun -n <name> -s                                   print what that session is showing
  cdrun -n <name> -q                                   drain, then end and archive the session
  cdrun -p <pid> <prompt> | - | -s | -q                the same, picking the session by PID
  cdrun -l [keyword ...]                               list running sessions and their PIDs
  cdrun -m <old-dir> <new-dir>                         after a working directory was renamed or
                                                       moved, move what Codex keeps for it

options:
  -d <dir>     working directory; created if missing
  -n <name>    name a session with -d (default: directory name), otherwise pick
               an exact name, ignoring case. With -d, reuse the same name and
               directory; refuse duplicate names or a different directory.
  -p <pid>     act on the running session with this broker PID
  -s           with -p or -n: print session output
  -q           drain queued work, then end and archive; may follow a prompt.
               With a prompt or -d, return once accepted; standalone waits
               for the session to end. Never interrupt work or impose a deadline.
  -l           list running sessions matching every keyword in name, directory
               or thread ID (case-insensitive)
  -m           move <old-dir>'s table in Codex's config.toml (its trust) to
               <new-dir>. Move the directory first; nothing is overwritten.

cdrun extensions: -s --json, --cancel, -d --trust=false, --version, /exit,
and discovered /skill-name commands. An idle session releases its conversation
for the Codex app. Turns run in the Codex app when it is installed, so its
windows and paired phones can follow them; CDRUN_CODEX_APP=0 keeps them here.
A prompt sent with -q runs here, so the session archives right after it.
A conversation the Codex app still holds at the end stays there to archive.
Native conversations cannot combine a prompt with -q;
archive them in their Codex app or CLI. -d prints only the PID.
Success exits 0; failure exits 1.`

func run(args []string) error {
	if len(args) == 2 && args[0] == "__wake" {
		return openWakeLink(args[1])
	}
	if len(args) > 0 && args[0] == "__observe" {
		if len(args) != 3 {
			return errors.New("invalid observer arguments")
		}
		return serveNative(args[1], args[2])
	}
	if len(args) > 0 && args[0] == "__serve" {
		if len(args) != 5 {
			return errors.New("invalid internal arguments")
		}
		trust, err := strconv.ParseBool(args[4])
		if err != nil {
			return err
		}
		return serve(args[1], args[2], args[3], trust)
	}
	fs := flag.NewFlagSet("cdrun", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("d", "", "working directory")
	name := fs.String("n", "", "session name")
	list := fs.Bool("l", false, "list running sessions")
	target := fs.String("p", "", "broker PID")
	screen := fs.Bool("s", false, "read session output")
	quit := fs.Bool("q", false, "end session")
	asJSON := fs.Bool("json", false, "structured status with -s")
	showVersion := fs.Bool("version", false, "print version")
	trust := fs.Bool("trust", true, "automatically trust this workspace for this private app-server only")
	cancel := fs.Bool("cancel", false, "interrupt the active turn; keep queued prompts")
	move := fs.Bool("m", false, "move what Codex keeps for a working directory that moved")
	if err := fs.Parse(quitArgs(args)); err != nil {
		if err == flag.ErrHelp {
			fmt.Println(usage)
			return nil
		}
		return fmt.Errorf("%w\n%s", err, usage)
	}
	if *showVersion {
		if len(args) != 1 {
			return errors.New("--version must be used alone")
		}
		fmt.Println("cdrun " + version)
		return nil
	}
	pidValue := 0
	if *target != "" {
		parsed, parseErr := strconv.ParseUint(*target, 10, 32)
		if parseErr != nil || parsed == 0 {
			return errors.New("invalid session target; use a positive broker PID")
		}
		pidValue = int(parsed)
	}
	pid := &pidValue
	trustSet := false
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		seen[f.Name] = true
		if f.Name == "trust" {
			trustSet = true
		}
	})
	if (seen["d"] && *dir == "") || (seen["n"] && strings.TrimSpace(*name) == "") || (seen["p"] && *target == "") {
		return errors.New(usage)
	}
	if *move {
		if len(seen) != 1 || fs.NArg() != 2 {
			return fmt.Errorf("-m takes the directory's old path and its new one, nothing else\n%s", usage)
		}
		return moveSettings(fs.Arg(0), fs.Arg(1))
	}
	if *list {
		if len(seen) != 1 {
			return fmt.Errorf("-l takes only keywords\n%s", usage)
		}
		for _, s := range fs.Args() {
			if strings.HasPrefix(s, "-") {
				return fmt.Errorf("unexpected argument %q\n%s", s, usage)
			}
		}
		return listSessions(fs.Args())
	}
	hasTarget := *pid != 0 || *name != ""
	if (trustSet && *dir == "") || (*cancel && (!hasTarget || *dir != "" || *screen || *quit || fs.NArg() > 0)) {
		return errors.New(usage)
	}
	if (*dir == "" && !hasTarget) || (*dir != "" && (*pid != 0 || *screen)) || (*screen && *quit) || (*asJSON && !*screen) || (*screen && (!hasTarget || fs.NArg() > 0)) || (*pid != 0 && *name != "") {
		return errors.New(usage)
	}
	for _, s := range fs.Args() {
		if strings.HasPrefix(s, "-") && s != "-" {
			return fmt.Errorf("unexpected argument %q; quote the whole prompt and put options before it\n%s", s, usage)
		}
	}
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "-" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
		if err != nil {
			return err
		}
		if len(data) > 65536 {
			return errors.New("stdin exceeds 65536 bytes")
		}
		prompt = string(data)
	}
	prompt, err := normalize(prompt)
	if err != nil {
		return err
	}
	if *dir != "" {
		return startOrReuse(*dir, *name, prompt, *trust, *quit)
	}
	if !*screen && !*quit && !*cancel && prompt == "" {
		return fmt.Errorf("nothing to inject\n%s", usage)
	}
	if *name != "" {
		*pid, err = pidByName(*name)
		if err != nil {
			return err
		}
	}
	ep, err := loadEndpoint(*pid)
	if err != nil {
		return err
	}
	if *cancel {
		empty := ""
		_, err = fetch(ep, "/cancel", &empty)
		return err
	}
	if *screen {
		data, err := fetch(ep, "/status", nil)
		if err != nil {
			return err
		}
		if *asJSON {
			fmt.Print(string(data))
			return nil
		}
		var s snapshot
		if err = json.Unmarshal(data, &s); err != nil {
			return err
		}
		fmt.Printf("[cdrun status=%s completed=%d queued=%d]\nthread=%s model=%s\n", s.Status, s.Completed, s.Queued, s.ThreadID, s.Model)
		for _, t := range s.Turns {
			fmt.Printf("\n> %s\n%s\n", t.Prompt, t.Answer)
			if t.Error != "" {
				fmt.Println("ERROR:", t.Error)
			}
		}
		if s.Status == "archiving" {
			fmt.Println("(archiving; if the Codex app still has this conversation open, the session ends and leaves it there)")
		}
		if s.Error != "" {
			fmt.Println("ERROR:", s.Error)
		}
		return nil
	}
	if *quit && prompt == "" {
		return quitSession(ep)
	}
	if prompt == "" {
		return errors.New("nothing to inject")
	}
	return submitPrompt(ep, prompt, *quit)
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cdrun:", err)
		os.Exit(1)
	}
}
