//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Codex app runs this broker's turns in its own app-server, the one its windows and paired
// phones talk to, so all of them can watch and steer a turn while it runs. The broker never
// becomes the writer for those turns: it puts each prompt into Codex's persistent thread queue,
// wakes the app with the app's background link codex://threads/wake/<thread>, and follows the
// turn through a private app-server that only reads. The prompt sent with -q is the exception
// (see deliver): the broker runs it itself, so it can archive the conversation right after.
var (
	appGrace    = 60 * time.Second // how long a woken app may take before the broker runs the prompt
	rewakeEvery = 15 * time.Second
	pollEvery   = 2 * time.Second
)

type codexTurn struct {
	ID, Status  string
	Error       json.RawMessage
	CompletedAt *int64
	Items       []struct {
		Type, Text string
		ClientID   string `json:"clientId"`
	}
}

// handOff queues the prompt for whichever app-server holds the conversation, wakes the Codex
// app when nobody does, and follows the resulting turn until it ends. When no app takes the
// prompt in time, the broker withdraws it from the queue and runs it itself. A final prompt
// (sent with -q) never wakes the app: once the conversation is free, the broker runs it.
func (b *broker) handOff(input []any, final bool) bool {
	wake := b.wake
	if final {
		wake = nil
	}
	client := newUUID()
	raw, err := b.r.call("thread/queue/add", object{"threadId": b.state.ThreadID, "clientUserMessageId": client, "input": input})
	if err != nil {
		return b.fatal(err)
	}
	var added struct{ QueuedSubmission struct{ ID string } }
	if err = json.Unmarshal(raw, &added); err != nil || added.QueuedSubmission.ID == "" {
		return b.fatal(errors.New("Codex did not identify the queued prompt"))
	}
	queued := added.QueuedSubmission.ID
	var wokeAt, lastWake time.Time
	lastSeen := time.Now()
	for {
		// Check the writer before reading: a writer writes a turn's end before it lets go.
		active := b.othersWriting()
		t, err := b.findTurn(client)
		if err != nil {
			return b.fatal(err)
		}
		if t != nil {
			b.showTurn(t)
			if t.CompletedAt != nil {
				failure := ""
				if len(t.Error) > 0 && string(t.Error) != "null" {
					failure = string(t.Error)
				}
				b.finishTurn(t.Status, failure)
				return true
			}
			if !active {
				b.finishTurn("interrupted", "the app-server running this turn stopped before it finished")
				return true
			}
		} else {
			waiting, err := b.inQueue(queued)
			if err != nil {
				return b.fatal(err)
			}
			switch {
			case waiting && active:
				// The writer is busy with earlier work; this prompt runs after it.
				lastSeen = time.Now()
			case waiting:
				lastSeen = time.Now()
				wakeFailed := false
				if wake != nil && (lastWake.IsZero() || time.Since(lastWake) >= rewakeEvery) && (wokeAt.IsZero() || time.Since(wokeAt) < appGrace) {
					wakeFailed = wake() != nil
					lastWake = time.Now()
					if wokeAt.IsZero() {
						wokeAt = lastWake
					}
				}
				if wake == nil || wakeFailed || time.Since(wokeAt) >= appGrace {
					taken, err := b.withdraw(queued)
					if err != nil {
						return b.fatal(err)
					}
					if taken {
						return b.runHere(input, client, final)
					}
				}
			default:
				// Neither queued nor started: a writer is starting it, or someone removed it.
				if !active && time.Since(lastSeen) > appGrace/2 {
					b.finishTurn("failed", "the prompt left the Codex queue without starting a turn")
					return true
				}
			}
		}
		select {
		case <-b.stop:
			return false
		case <-time.After(pollEvery):
		}
	}
}

// runHere runs a withdrawn prompt in this broker's app-server, or queues it again when another
// writer took the conversation in the meantime.
func (b *broker) runHere(input []any, client string, final bool) bool {
	held, err := b.resume()
	if err != nil {
		return b.fatal(err)
	}
	if !held {
		return b.handOff(input, final)
	}
	return b.startTurn(input, client)
}

func (b *broker) findTurn(client string) (*codexTurn, error) {
	raw, err := b.r.call("thread/turns/list", object{"threadId": b.state.ThreadID, "limit": maxTurns, "sortDirection": "desc", "itemsView": "summary"})
	if err != nil {
		return nil, err
	}
	var list struct{ Data []codexTurn }
	if err = json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	for i := range list.Data {
		for _, item := range list.Data[i].Items {
			if item.Type == "userMessage" && item.ClientID == client {
				return &list.Data[i], nil
			}
		}
	}
	return nil, nil
}

func (b *broker) showTurn(t *codexTurn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := &b.state.Turns[len(b.state.Turns)-1]
	cur.ID, cur.Answer, cur.Truncated = t.ID, "", false
	for _, item := range t.Items {
		if item.Type == "agentMessage" && item.Text != "" {
			if cur.Answer != "" {
				appendAnswer(cur, "\n")
			}
			appendAnswer(cur, item.Text)
		}
	}
	b.state.Status = "running"
}

func (b *broker) queuedIDs() (map[string]bool, error) {
	ids := map[string]bool{}
	params := object{"threadId": b.state.ThreadID}
	for {
		raw, err := b.r.call("thread/queue/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data       []struct{ ID string }
			NextCursor *string
		}
		if err = json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		for _, q := range page.Data {
			ids[q.ID] = true
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return ids, nil
		}
		params = object{"threadId": b.state.ThreadID, "cursor": *page.NextCursor}
	}
}

func (b *broker) inQueue(id string) (bool, error) {
	ids, err := b.queuedIDs()
	return ids[id], err
}

func (b *broker) queuedInCodex() (int, error) {
	ids, err := b.queuedIDs()
	return len(ids), err
}

// withdraw takes a queued prompt back; false means a writer took it first.
func (b *broker) withdraw(id string) (bool, error) {
	_, err := b.r.call("thread/queue/delete", object{"threadId": b.state.ThreadID, "queuedSubmissionId": id})
	if err == nil {
		return true, nil
	}
	still, qerr := b.inQueue(id)
	if qerr != nil {
		return false, qerr
	}
	if still {
		return false, err
	}
	return false, nil
}

// The broker's Windows Job Object ends everything in it with the broker. An app started by the
// wake link must never belong to it, so a helper outside the job opens the link.
var (
	jobMu     sync.Mutex
	brokerJob windows.Handle
)

func setJobLimits(flags uint32) error {
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = flags
	_, err := windows.SetInformationJobObject(brokerJob, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}

// startOutsideJob allows breakaway only while this one process starts; when an enclosing job
// forbids breakaway, starting fails and the broker runs the prompt itself instead.
func startOutsideJob(cmd *exec.Cmd) error {
	jobMu.Lock()
	defer jobMu.Unlock()
	if brokerJob != 0 {
		if err := setJobLimits(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK); err != nil {
			return err
		}
		defer setJobLimits(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
	}
	return cmd.Start()
}

// wakeCodexApp asks the Codex app to run this thread's queued prompts. The app opens the
// conversation in its own app-server without showing a window, starting in the background
// when it is not running.
func wakeCodexApp(threadID string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "__wake", threadID)
	cmd.Env = cleanEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB}
	if err = startOutsideJob(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		return err
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("timed out waking the Codex app")
	}
}

// shellExecuteInfo mirrors SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size       uint32
	mask       uint32
	hwnd       windows.Handle
	verb       *uint16
	file       *uint16
	parameters *uint16
	directory  *uint16
	show       int32
	instApp    windows.Handle
	idList     uintptr
	class      *uint16
	keyClass   windows.Handle
	hotKey     uint32
	icon       windows.Handle
	process    windows.Handle
}

const (
	seeMaskNoAsync  = 0x00000100
	seeMaskFlagNoUI = 0x00000400
)

var procShellExecuteEx = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// openWakeLink runs in the __wake helper. Without the app installed it fails quietly, never
// showing Windows' "choose an app" dialog.
func openWakeLink(id string) error {
	if !validThreadID(id) {
		return errors.New("invalid thread identifier")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE) == nil {
		defer windows.CoUninitialize()
	}
	verb, _ := windows.UTF16PtrFromString("open")
	link, _ := windows.UTF16PtrFromString("codex://threads/wake/" + strings.ToLower(id))
	info := shellExecuteInfo{mask: seeMaskNoAsync | seeMaskFlagNoUI, verb: verb, file: link, show: windows.SW_SHOWNOACTIVATE}
	info.size = uint32(unsafe.Sizeof(info))
	if ok, _, err := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		return err
	}
	return nil
}
