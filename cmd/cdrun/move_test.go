//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// movedFixture is a renamed directory and a config.toml that still names its old path.
func movedFixture(t *testing.T, config string) (from, to, file string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	work := t.TempDir()
	from, to = filepath.Join(work, "ledger"), filepath.Join(work, "books")
	if err := os.Mkdir(to, 0o755); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(home, "config.toml")
	config = strings.ReplaceAll(config, "FROM", strings.ToLower(from))
	config = strings.ReplaceAll(config, "TO", strings.ToLower(to))
	if err := os.WriteFile(file, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return from, to, file
}

func TestMoveSettings(t *testing.T) {
	from, to, file := movedFixture(t, "model = \"x\"\r\n\r\n[projects.'FROM'] # trusted by hand\r\ntrust_level = \"trusted\"\r\n\r\n[projects.'c:\\other']\r\ntrust_level = \"trusted\"\r\n")
	if err := moveSettings(from, to); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	want := "model = \"x\"\r\n\r\n[projects.'" + strings.ToLower(to) + "']\r\ntrust_level = \"trusted\"\r\n\r\n[projects.'c:\\other']\r\ntrust_level = \"trusted\"\r\n"
	if string(data) != want {
		t.Fatalf("config.toml is\n%q\nwant\n%q", data, want)
	}
	if _, err := os.Stat(file + ".cdrun-tmp"); !os.IsNotExist(err) {
		t.Fatalf("left its temporary file behind: %v", err)
	}
}

func TestMoveSettingsBasicStringKey(t *testing.T) {
	from, to, file := movedFixture(t, "")
	escaped := strings.ReplaceAll(strings.ToLower(from), `\`, `\\`)
	os.WriteFile(file, []byte("[projects.\""+escaped+"\"]\ntrust_level = \"trusted\"\n"), 0o644)
	if err := moveSettings(from, to); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); string(data) != "[projects.'"+strings.ToLower(to)+"']\ntrust_level = \"trusted\"\n" {
		t.Fatalf("config.toml is %q", data)
	}
}

func TestMoveSettingsNothingKept(t *testing.T) {
	from, to, file := movedFixture(t, "[projects.'c:\\other']\ntrust_level = \"trusted\"\n")
	if err := moveSettings(from, to); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(file); string(data) != "[projects.'c:\\other']\ntrust_level = \"trusted\"\n" {
		t.Fatalf("config.toml changed: %q", data)
	}
	os.Remove(file)
	if err := moveSettings(from, to); err != nil {
		t.Fatalf("no config.toml at all: %v", err)
	}
}

func TestMoveSettingsNeverOverwrites(t *testing.T) {
	config := "[projects.'FROM']\ntrust_level = \"trusted\"\n[projects.'TO']\ntrust_level = \"untrusted\"\n"
	from, to, file := movedFixture(t, config)
	before, _ := os.ReadFile(file)
	if err := moveSettings(from, to); err == nil || !strings.Contains(err.Error(), "already has settings") {
		t.Fatalf("moveSettings over existing settings: %v", err)
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("config.toml changed: %q", after)
	}
}

func TestMoveSettingsAfterTheDirectoryOnly(t *testing.T) {
	from, to, _ := movedFixture(t, "")
	if err := moveSettings(to, filepath.Join(filepath.Dir(to), "nope")); err == nil || !strings.Contains(err.Error(), "move the directory first") {
		t.Fatalf("to a directory that does not exist: %v", err)
	}
	os.Mkdir(from, 0o755)
	if err := moveSettings(from, to); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("from a directory that is still there: %v", err)
	}
}

func TestMoveFlag(t *testing.T) {
	for _, args := range [][]string{{"-m", "a"}, {"-m", "a", "b", "c"}, {"-m", "-n", "x", "a", "b"}, {"-m", "-l", "a", "b"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "-m takes") {
			t.Errorf("run(%q) = %v", args, err)
		}
	}
}
