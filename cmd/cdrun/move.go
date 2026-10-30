//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// projectTableRE matches a [projects.<key>] table header in config.toml, its key
// a literal or a basic string.
var projectTableRE = regexp.MustCompile(`^\s*\[\s*projects\s*\.\s*(?:'([^']*)'|("(?:[^"\\]|\\.)*"))\s*\]\s*(?:#.*)?$`)

// moveSettings carries the settings Codex keeps for a working directory -- the
// [projects.'<dir>'] table in config.toml, where it records that the directory
// is trusted -- over to the directory's new path. cdrun's own sessions trust
// their directory by themselves, but the rounds the Codex app takes over run
// with the app's settings, and to those a renamed directory is a stranger. The
// directory has to have moved already: its settings follow it.
func moveSettings(from, to string) error {
	from, err := filepath.Abs(from)
	if err != nil {
		return err
	}
	if to, err = filepath.Abs(to); err != nil {
		return err
	}
	if info, err := os.Stat(to); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory; move the directory first, then its settings", to)
	}
	if _, err := os.Stat(from); err == nil && !sameDirectory(from, to) {
		return fmt.Errorf("%s still exists; -m is for after the directory has moved", from)
	}
	file := filepath.Join(codexHome(), "config.toml")
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "codex keeps no settings for %s; nothing to move\n", from)
		return nil
	} else if err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	at, old, taken := -1, "", false
	for i, line := range lines {
		key, ok := projectKey(line)
		switch {
		case !ok:
		case sameDirectory(key, from):
			at, old = i, key
		case sameDirectory(key, to):
			taken = true
		}
	}
	if at < 0 {
		fmt.Fprintf(os.Stderr, "codex keeps no settings for %s; nothing to move\n", from)
		return nil
	}
	if taken {
		return fmt.Errorf("%s already has settings for %s; merge or remove them, then run this again", file, to)
	}
	// Codex writes its keys in lower case on Windows; a new one follows suit.
	key := to
	if old == strings.ToLower(old) {
		key = strings.ToLower(to)
	}
	header := "[projects." + strconv.Quote(key) + "]"
	if !strings.ContainsAny(key, "'\r\n") {
		header = "[projects.'" + key + "']"
	}
	if strings.HasSuffix(lines[at], "\r") {
		header += "\r"
	}
	lines[at] = header
	// Written beside it and renamed over it, so that the Codex app never reads
	// half a file.
	tmp := file + ".cdrun-tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Fprintf(os.Stderr, "moved codex's settings for %s to %s\n", from, key)
	return nil
}

// projectKey returns the directory a [projects.<key>] table header names.
func projectKey(line string) (string, bool) {
	m := projectTableRE.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
	if m == nil {
		return "", false
	}
	if m[2] == "" {
		return m[1], true
	}
	key, err := strconv.Unquote(m[2])
	return key, err == nil
}
