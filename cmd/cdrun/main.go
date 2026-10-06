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

// A broker owns one Codex conversation, but holds it only while it has work. Codex lets a
// single app-server write to a conversation at a time, so once the broker is idle it stops
// its app-server and the conversation is free for the Codex app (and a phone paired with it).
// A later prompt takes the conversation back, or, when the Codex app has it open, is queued
// into that app's conversation.
type broker struct {
	mu       sync.Mutex
	state    snapshot
	r        *rpc
	queue    chan string
	finished chan turn
	stop     chan struct{}
	once     sync.Once
	events   *os.File
	closing  bool
	inputs   func(string) ([]any, error)
	connect  func() (*rpc, error) // starts a fresh app-server; nil keeps r as it is
	release  func()               // stops it again, freeing the conversation
	holding  bool                 // this broker's app-server is the conversation's writer
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
			if !b.deliver(prompt) {
				return
			}
			b.releaseWhenIdle()
		}
	}
}

// deliver runs one prompt: in this broker's app-server when it can hold the conversation,
// otherwise in the queue of the Codex app that has it open. It returns false after a fatal
// failure; queued work is then dropped, never replayed.
func (b *broker) deliver(prompt string) bool {
	input, err := b.prepare(prompt)
	if err != nil {
		b.finishTurn("failed", err.Error())
		return true
	}
	if !b.holding {
		held, err := b.resume()
		if err != nil {
			return b.fatal(err)
		}
		if !held {
			if _, err = b.r.call("thread/queue/add", object{"threadId": b.state.ThreadID, "clientUserMessageId": newUUID(), "input": input}); err != nil {
				return b.fatal(err)
			}
			b.finishTurn("delegated", "")
			return true
		}
	}
	r := b.r
	if _, err = r.call("turn/start", object{"threadId": b.state.ThreadID, "input": input}); err != nil {
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
	if b.r != nil {
		select {
		case <-b.r.dead:
		default:
			return b.input(prompt)
		}
	}
	if b.connect == nil {
		return nil, errors.New("Codex app-server disconnected")
	}
	r, err := b.connect()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.r, b.holding = r, false
	b.mu.Unlock()
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

// archive ends the conversation the way the Codex app does. While the Codex app has it open,
// Codex refuses, and the conversation simply stays there for the user.
func (b *broker) archive() {
	if b.r == nil {
		if b.connect == nil {
			return
		}
		r, err := b.connect()
		if err != nil {
			return
		}
		b.mu.Lock()
		b.r = r
		b.mu.Unlock()
	}
	if _, err := b.r.call("thread/archive", object{"threadId": b.state.ThreadID}); err != nil {
		b.mu.Lock()
		b.state.Error = "conversation not archived: " + err.Error()
		b.mu.Unlock()
	}
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
		case r.Method == "POST" && r.URL.Path == "/prompt":
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
			if prompt == "/exit" && b.state.Error != "" {
				b.shutdown()
				_ = json.NewEncoder(w).Encode(object{"closing": true})
				return
			}
			if b.closing || b.state.Status == "closed" || (b.state.Error != "" && prompt != "/exit") {
				http.Error(w, "session unavailable: "+b.state.Error, 409)
				return
			}
			select {
			case b.queue <- prompt:
				b.state.Queued++
				if prompt == "/exit" {
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
			r := b.r
			b.mu.Unlock()
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
	if _, err = r.call("initialize", object{"clientInfo": object{"name": "cdrun", "title": "cdrun", "version": version}, "capabilities": object{"experimentalApi": true}}); err != nil {
		s.stopLocked()
		return nil, err
	}
	if err = r.send(object{"method": "initialized", "params": object{}}); err != nil {
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
	_ = job
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
	ep := endpoint{PID: os.Getpid(), URL: "http://" + listener.Addr().String(), Token: hex.EncodeToString(token), Dir: dir, Name: name}
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
	go b.run()
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
	httpServer.Close()
	b.archive()
	b.mu.Lock()
	b.state.Status = "closed"
	_ = writeJSON(filepath.Join(sessionDir, "final.json"), b.state)
	b.mu.Unlock()
	return nil
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
func launch(dir, name, prompt string, trust bool) error {
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
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(stateRoot(), "launch-*.log")
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(exe, "__serve", abs, name, codexExe, strconv.FormatBool(trust))
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
				if prompt != "" {
					if _, err = fetch(e, "/prompt", &prompt); err != nil {
						empty := ""
						_, _ = fetch(e, "/force-close", &empty)
						return err
					}
				}
				fmt.Println(pid)
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

const version = "0.1.2"
const usage = `cdrun ` + version + ` - drive a headless Codex session by broker PID
  cdrun -d <dir> [-r <name>] [--trust] [prompt]
  cdrun -p <pid> <prompt|->
  cdrun -p <pid> -s [--json]
  cdrun -p <pid> -q
  cdrun -p <pid> --cancel
Launch prints only the broker PID. /exit is handled by cdrun. Not a desktop UI controller.
An idle session lets go of its conversation, so it can be continued in the Codex app.`

func run(args []string) error {
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
	name := fs.String("r", "", "session name")
	target := fs.String("p", "", "broker PID")
	screen := fs.Bool("s", false, "read session output")
	quit := fs.Bool("q", false, "end session")
	asJSON := fs.Bool("json", false, "structured status with -s")
	showVersion := fs.Bool("version", false, "print version")
	trust := fs.Bool("trust", true, "automatically trust this workspace for this private app-server only")
	cancel := fs.Bool("cancel", false, "interrupt the active turn; keep queued prompts")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Println(usage)
			return nil
		}
		return err
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
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "trust" {
			trustSet = true
		}
	})
	if (trustSet && *dir == "") || (*cancel && (*pid == 0 || *screen || *quit || fs.NArg() > 0)) {
		return errors.New(usage)
	}
	if (*dir == "") == (*pid == 0) || (*screen && *quit) || (*asJSON && !*screen) || ((*screen || *quit) && (*pid == 0 || fs.NArg() > 0)) || (*pid != 0 && *name != "") {
		return errors.New(usage)
	}
	for _, s := range fs.Args() {
		if strings.HasPrefix(s, "-") && s != "-" {
			return fmt.Errorf("unexpected argument %q; quote the whole prompt and put options before it", s)
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
		return launch(*dir, *name, prompt, *trust)
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
			if t.Status == "delegated" {
				fmt.Println("(queued in the Codex app, which has this conversation open)")
			}
			if t.Error != "" {
				fmt.Println("ERROR:", t.Error)
			}
		}
		if s.Error != "" {
			fmt.Println("ERROR:", s.Error)
		}
		return nil
	}
	if *quit {
		handle, openErr := openVerified(ep, true)
		if openErr != nil {
			return openErr
		}
		defer windows.CloseHandle(handle)
		exit := "/exit"
		_, err = fetch(ep, "/prompt", &exit)
		if result, waitErr := windows.WaitForSingleObject(handle, 10000); waitErr != nil {
			return waitErr
		} else if result == windows.WAIT_OBJECT_0 {
			return nil
		}
		empty := ""
		_, err = fetch(ep, "/force-close", &empty)
		if err != nil {
			_ = windows.TerminateProcess(handle, 1)
		}
		if result, waitErr := windows.WaitForSingleObject(handle, 5000); waitErr != nil {
			return waitErr
		} else if result != windows.WAIT_OBJECT_0 {
			if err = windows.TerminateProcess(handle, 1); err != nil {
				return err
			}
			if result, err = windows.WaitForSingleObject(handle, 5000); err != nil || result != windows.WAIT_OBJECT_0 {
				return errors.New("session did not close")
			}
		}
		return nil
	}
	if prompt == "" {
		return errors.New("nothing to inject")
	}
	_, err = fetch(ep, "/prompt", &prompt)
	return err
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cdrun:", err)
		os.Exit(1)
	}
}
