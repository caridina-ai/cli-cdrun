package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNativeWriterRequiresHeldLock(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	id := "12345678-1234-1234-1234-123456789abc"
	if active, err := nativeWriterActive(id); err != nil || active {
		t.Fatal(active, err)
	}
	dir := filepath.Join(codexHome(), "thread-writer-locks")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, id+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if active, err := nativeWriterActive(id); err != nil || active {
		t.Fatal("stale unlocked file listed", active, err)
	}
	var overlap windows.Overlapped
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap); err != nil {
		t.Fatal(err)
	}
	defer windows.UnlockFileEx(h, 0, 1, 0, &overlap)
	if active, err := nativeWriterActive(id); err != nil || !active {
		t.Fatal("live native writer omitted", active, err)
	}
	for _, invalid := range []string{"../other", ".coordination", "", "12345678-1234-1234-1234-123456789abz"} {
		if _, err := nativeWriterActive(invalid); err == nil {
			t.Fatal("invalid thread identifier accepted", invalid)
		}
	}
}

func TestRegistryLockIsReentrantOnPinnedThread(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	unlock, err := lockRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	inner, err := lockRegistry()
	if err != nil {
		t.Fatal(err)
	}
	inner()
}

func TestReadNativeNameDoesNotResumeOrRename(t *testing.T) {
	b, _ := newIdleBroker()
	f := newFakeServer(t, b, func(method string) (string, string) {
		if method != "thread/read" {
			t.Errorf("unexpected mutation: %s", method)
		}
		return `{"thread":{"id":"fixture","name":"00919","cwd":"D:\\work","originator":"Codex Desktop"}}`, ""
	})
	thread, err := readNativeThread(f.r, "fixture")
	if err != nil || thread.Name != "00919" || thread.Cwd != `D:\work` {
		t.Fatal(thread, err)
	}
}
