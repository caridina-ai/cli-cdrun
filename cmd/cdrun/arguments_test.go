package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSplitAndInterspersedArguments(t *testing.T) {
	for _, tt := range []struct {
		args, prompt, forwarded []string
		name                    string
		quit                    bool
	}{
		{[]string{"-d", "dir", "hello", "-n", "bot", "-q", "--", "-c", `value="a b"`, "--", "-q"}, []string{"hello"}, []string{"-c", `value="a b"`, "--", "-q"}, "bot", true},
		{[]string{"hello", "-d=dir", "world", "-n", "-q", "--"}, []string{"hello", "world"}, []string{}, "-q", false},
		{[]string{"-d", "dir", "-", "-q"}, []string{"-"}, nil, "", true},
		{[]string{"-d", "dir", "--", "-c", "", `path=C:\space dir\`, `quoted="hi"`}, nil, []string{"-c", "", `path=C:\space dir\`, `quoted="hi"`}, "", false},
	} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("d", "", "")
		name := fs.String("n", "", "")
		quit := fs.Bool("q", false, "")
		local, forwarded := splitCodexArgs(tt.args)
		ordered, err := interspersedArgs(fs, local)
		if err != nil {
			t.Fatal(err)
		}
		if err := fs.Parse(ordered); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(fs.Args(), tt.prompt) || !slices.Equal(forwarded, tt.forwarded) || *name != tt.name || *quit != tt.quit {
			t.Fatalf("%q: prompt=%q forwarded=%q name=%q quit=%v", tt.args, fs.Args(), forwarded, *name, *quit)
		}
	}
}

func TestForwardingRequiresNewSession(t *testing.T) {
	dir := t.TempDir()
	ep := fixtureSession(t, "existing", dir)
	b, _ := newIdleBroker()
	s := httptest.NewServer(b.handler(endpoint{Token: ep.Token}))
	defer s.Close()
	b.state = snapshot{PID: ep.PID, Name: "existing", Cwd: dir, Status: "released"}
	ep.URL = s.URL
	if err := writeJSON(endpointPath(ep.PID), ep); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-p", strconv.Itoa(ep.PID), "hello"}, {"-n", "existing", "hello"},
		{"-n", "existing", "-s"}, {"-n", "existing", "-q"}, {"-l"},
		{"-m", "old", "new"}, {"--help"}, {"--version"},
		{"-d", dir, "-n", "existing", "hello", "-q"},
	} {
		args = append(args, "--", "-c", "model=fixture")
		if err := run(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	if len(b.queue) != 0 || b.closing {
		t.Fatal("rejected forwarding submitted work or requested shutdown")
	}
	if err := run([]string{"-d", dir, "-n", "existing", "--"}); err != nil {
		t.Fatal("empty separator should permit reuse:", err)
	}
	for _, arg := range []string{"--stdio", "--listen", "--listen=ws://127.0.0.1:0"} {
		if err := run([]string{"-d", dir, "--", arg}); err == nil || !strings.Contains(err.Error(), "transport") {
			t.Fatalf("transport conflict %q: %v", arg, err)
		}
	}
}

// This subprocess speaks enough JSON-RPC to verify the actual Windows argv boundary.
func TestMain(m *testing.M) {
	if os.Getenv("CDRUN_TEST_ARGV") == "" {
		os.Exit(m.Run())
	}
	data, _ := json.Marshal(os.Args[1:])
	if err := os.WriteFile(os.Getenv("CDRUN_TEST_ARGV"), data, 0600); err != nil {
		os.Exit(2)
	}
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		var msg wire
		if json.Unmarshal(input.Bytes(), &msg) == nil && len(msg.ID) > 0 {
			_ = json.NewEncoder(os.Stdout).Encode(object{"id": msg.ID, "result": object{}})
		}
	}
	os.Exit(0)
}

func TestAppServerPreservesArgumentsOnRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("CDRUN_TEST_ARGV", file)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-c", `model="fixture model"`, "-c", `path="C:\space dir\\"`, "", `quotes="hello"`, "--", "-q"}
	s := &appServer{exe: exe, dir: t.TempDir(), trust: true, args: args, stderr: io.Discard}
	defer s.stop()
	for i := 0; i < 2; i++ {
		if _, err := s.start(func(wire) {}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		key, _ := json.Marshal(s.dir)
		want := append([]string{"app-server", "--stdio"}, args...)
		want = append(want, "-c", "projects."+string(key)+".trust_level=\"trusted\"")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		s.stop()
	}
}
