//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var skillCommand = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Preserve the original slash command and arguments for the skill's workflow.
func skillInput(r *rpc, dir, prompt string) ([]any, error) {
	input := []any{object{"type": "text", "text": prompt}}
	fields := strings.Fields(prompt)
	if len(fields) == 0 || !skillCommand.MatchString(fields[0]) {
		return input, nil
	}
	command := fields[0]
	name := strings.TrimPrefix(command, "/")
	raw, err := r.call("skills/list", object{"cwds": []string{dir}, "forceReload": true})
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []struct {
			Skills []struct {
				Name, Path string
				Enabled    bool
			}
		}
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	var paths []string
	for _, group := range result.Data {
		for _, skill := range group.Skills {
			if skill.Name == name && skill.Enabled {
				paths = append(paths, skill.Path)
			}
		}
	}
	if len(paths) != 1 {
		return nil, fmt.Errorf("%s requires exactly one enabled Codex skill named %q; found %d", command, name, len(paths))
	}
	return append(input, object{"type": "skill", "name": name, "path": paths[0]}), nil
}
