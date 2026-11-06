package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

func codexHome() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func validThreadID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// Probe the writer's existing byte-range lock without reading or changing its file.
// File existence alone is insufficient: a crash can leave an unlocked file behind.
func nativeWriterActive(id string) (bool, error) {
	if !validThreadID(id) {
		return false, errors.New("invalid thread identifier")
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(codexHome(), "thread-writer-locks", id+".lock"))
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	var overlapped windows.Overlapped
	err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err == windows.ERROR_LOCK_VIOLATION {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, windows.UnlockFileEx(h, 0, 1, 0, &overlapped)
}

type nativeThread struct {
	ID, Name, Cwd, Model, Originator, Preview string
}

func readNativeThread(r *rpc, id string) (nativeThread, error) {
	var response struct{ Thread nativeThread }
	raw, err := r.call("thread/read", object{"threadId": id})
	if err != nil {
		return response.Thread, err
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return response.Thread, err
	}
	if response.Thread.ID != id || response.Thread.Cwd == "" {
		return response.Thread, errors.New("invalid thread metadata")
	}
	if response.Thread.Name == "" {
		response.Thread.Name = response.Thread.Preview
		if response.Thread.Name == "" {
			response.Thread.Name = filepath.Base(response.Thread.Cwd)
		}
	}
	return response.Thread, nil
}

// Each native thread receives a real, private proxy PID. A desktop process may
// own many threads, so its OS PID is never used as a conversation selector.
func discoverNativeSessions() error {
	paths, err := filepath.Glob(filepath.Join(codexHome(), "thread-writer-locks", "*.lock"))
	if err != nil || len(paths) == 0 {
		return err
	}
	unlock, err := lockRegistry()
	if err != nil {
		return err
	}
	defer unlock()
	known := make(map[string]bool)
	brokers, err := brokerSessions()
	if err != nil {
		return err
	}
	for _, b := range brokers {
		known[b.ThreadID] = true
	}
	var server *appServer
	var r *rpc
	defer func() {
		if server != nil {
			server.stop()
		}
	}()
	for _, path := range paths {
		id := strings.TrimSuffix(filepath.Base(path), ".lock")
		if !validThreadID(id) || known[id] {
			continue
		}
		active, err := nativeWriterActive(id)
		if err != nil {
			return fmt.Errorf("inspect native thread %s: %w", id, err)
		}
		if !active {
			continue
		}
		if r == nil {
			exe, err := resolveCodex()
			if err != nil {
				return err
			}
			server = &appServer{exe: exe, dir: codexHome(), stderr: io.Discard}
			if r, err = server.start(func(wire) {}); err != nil {
				return err
			}
		}
		_, err = readNativeThread(r, id)
		if err != nil {
			if active, lockErr := nativeWriterActive(id); lockErr == nil && !active {
				continue
			}
			return fmt.Errorf("read native thread %s: %w", id, err)
		}
		if err := secureStateRoot(); err != nil {
			return err
		}
		if err := launchProcess([]string{"__observe", id, server.exe}, "", false, false); err != nil {
			if active, lockErr := nativeWriterActive(id); lockErr == nil && !active {
				continue
			}
			return err
		}
	}
	return nil
}

type nativeObserver struct {
	mu      sync.Mutex
	id      string
	server  *appServer
	state   snapshot
	updated time.Time
	done    chan struct{}
	once    sync.Once
}

// Serialize discovery with owned-thread startup, before its endpoint is published.
func lockRegistry() (func(), error) {
	runtime.LockOSThread()
	key := fmt.Sprintf(`Local\cdrun-discovery-%x`, sha256.Sum256([]byte(strings.ToLower(stateRoot()))))
	keyPtr, _ := windows.UTF16PtrFromString(key)
	h, err := windows.CreateMutex(nil, false, keyPtr)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		runtime.UnlockOSThread()
		return nil, err
	}
	result, err := windows.WaitForSingleObject(h, 120000)
	if err != nil || (result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED) {
		windows.CloseHandle(h)
		runtime.UnlockOSThread()
		if err == nil {
			err = errors.New("native session discovery timed out")
		}
		return nil, err
	}
	return func() { windows.ReleaseMutex(h); windows.CloseHandle(h); runtime.UnlockOSThread() }, nil
}

func (o *nativeObserver) close() { o.once.Do(func() { close(o.done) }) }

// A fresh private RPC client reads or queues work without resuming the thread.
func (o *nativeObserver) client() (*rpc, error) { return o.server.start(func(wire) {}) }

func (o *nativeObserver) status() (snapshot, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	active, err := nativeWriterActive(o.id)
	if err != nil {
		return snapshot{}, err
	}
	if !active {
		return snapshot{}, errors.New("native session no longer has an active writer")
	}
	if time.Since(o.updated) < time.Second {
		return o.state, nil
	}
	r, err := o.client()
	if err != nil {
		return snapshot{}, err
	}
	defer o.server.stop()
	thread, err := readNativeThread(r, o.id)
	if err != nil {
		return snapshot{}, err
	}
	s := snapshot{PID: os.Getpid(), ThreadID: o.id, Cwd: thread.Cwd, Name: thread.Name, Model: thread.Model, Status: "open", Queued: -1, Turns: []turn{}, Native: true}
	raw, err := r.call("thread/turns/list", object{"threadId": o.id, "limit": maxTurns, "sortDirection": "desc", "itemsView": "summary"})
	if err != nil {
		return snapshot{}, err
	}
	var response struct {
		Data []struct {
			ID, Status string
			Error      json.RawMessage
			Items      []struct {
				Type, Text string
				Content    []struct{ Type, Text string }
			}
		}
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return snapshot{}, err
	}
	for i := len(response.Data) - 1; i >= 0; i-- {
		v := response.Data[i]
		t := turn{ID: v.ID, Status: v.Status, Sequence: len(s.Turns) + 1}
		for _, item := range v.Items {
			if item.Type == "agentMessage" {
				appendAnswer(&t, item.Text)
			}
			if item.Type == "userMessage" {
				for _, content := range item.Content {
					if content.Type == "text" {
						t.Prompt += content.Text
					}
				}
			}
		}
		if text := []rune(t.Prompt); len(text) > 8000 {
			t.Prompt = string(text[:8000])
			t.Truncated = true
		}
		if len(v.Error) > 0 && string(v.Error) != "null" {
			t.Error = string(v.Error)
		}
		if v.Status == "inProgress" {
			s.Status = "running"
		} else {
			s.Completed++
		}
		s.Turns = append(s.Turns, t)
	}
	if len(s.Turns) > 0 {
		s.FirstSequence = 1
	}
	if raw, err := r.call("thread/queue/list", object{"threadId": o.id}); err == nil {
		var queue struct {
			Data       []json.RawMessage
			NextCursor *string
		}
		if json.Unmarshal(raw, &queue) == nil && queue.NextCursor == nil {
			s.Queued = len(queue.Data)
		}
	}
	o.state, o.updated = s, time.Now()
	return s, nil
}

func (o *nativeObserver) action(path, prompt string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if path != "/archive" {
		active, err := nativeWriterActive(o.id)
		if err != nil {
			return err
		}
		if !active {
			return errors.New("native session no longer has an active writer")
		}
	}
	r, err := o.client()
	if err != nil {
		return err
	}
	defer o.server.stop()
	switch path {
	case "/prompt":
		input, err := skillInput(r, o.state.Cwd, prompt)
		if err != nil {
			return err
		}
		_, err = r.call("thread/queue/add", object{"threadId": o.id, "clientUserMessageId": newUUID(), "input": input})
		return err
	case "/archive":
		if _, err := r.call("thread/archive", object{"threadId": o.id}); err != nil {
			return fmt.Errorf("conversation not archived: %w", err)
		}
		o.close()
		return nil
	default:
		return errors.New("this native session cannot be interrupted by a private client; use its Codex app or CLI")
	}
}

func (o *nativeObserver) handler(ep endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+ep.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method == "GET" && r.URL.Path == "/status" {
			s, err := o.status()
			if err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			json.NewEncoder(w).Encode(s)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "not found", 404)
			return
		}
		if r.URL.Path == "/force-close" {
			json.NewEncoder(w).Encode(object{"closing": true})
			o.close()
			return
		}
		if r.URL.Path != "/prompt" && r.URL.Path != "/archive" && r.URL.Path != "/cancel" {
			http.Error(w, "not found", 404)
			return
		}
		var q request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid request", 400)
			return
		}
		prompt, err := normalize(q.Prompt)
		if err != nil || (r.URL.Path == "/prompt" && prompt == "") {
			http.Error(w, "invalid prompt", 400)
			return
		}
		path := r.URL.Path
		if prompt == "/exit" {
			path = "/archive"
		}
		if err = o.action(path, prompt); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		json.NewEncoder(w).Encode(object{"accepted": true})
	}
}

func serveNative(id, exe string) error {
	if err := secureStateRoot(); err != nil {
		return err
	}
	if _, err := ownJob(); err != nil {
		return err
	}
	o := &nativeObserver{id: id, server: &appServer{exe: exe, dir: codexHome(), stderr: io.Discard}, done: make(chan struct{})}
	defer o.server.stop()
	s, err := o.status()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	ep := endpoint{PID: os.Getpid(), URL: "http://" + listener.Addr().String(), Token: hex.EncodeToString(token), Dir: s.Cwd, Name: s.Name, ThreadID: id, Native: true}
	if ep.Created, err = processBirth(windows.CurrentProcess()); err != nil {
		return err
	}
	if ep.Executable, err = os.Executable(); err != nil {
		return err
	}
	server := &http.Server{Handler: o.handler(ep), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 4 * time.Minute}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		server.Close()
	}()
	go func() { server.Serve(listener); o.close() }()
	if err = writeJSON(endpointPath(ep.PID), ep); err != nil {
		return err
	}
	defer os.Remove(endpointPath(ep.PID))
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-o.done:
			return nil
		case <-ticker.C:
			o.mu.Lock()
			active, err := nativeWriterActive(id)
			o.mu.Unlock()
			if err == nil && !active {
				return nil
			}
		}
	}
}
