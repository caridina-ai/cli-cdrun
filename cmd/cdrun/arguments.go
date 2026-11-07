package main

import (
	"flag"
	"fmt"
	"strings"
)

// splitCodexArgs preserves the caller's argument boundaries after the first separator.
func splitCodexArgs(args []string) (local, codex []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// interspersedArgs lets flag parse options on either side of positional arguments.
// A value belonging to an option is never examined as another option.
func interspersedArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, _, inline := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		f := fs.Lookup(name)
		if f == nil && name != "h" && name != "help" {
			return nil, fmt.Errorf("unknown option %q; quote the whole prompt", arg)
		}
		options = append(options, arg)
		if f == nil || inline {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("option %s needs a value", arg)
		}
		i++
		options = append(options, args[i])
	}
	return append(options, positional...), nil
}

// The broker requires its private stdio transport. Other options belong to Codex.
func validateCodexArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "--config" || arg == "--enable" || arg == "--disable" {
			i++
			continue
		}
		if arg == "--stdio" || strings.HasPrefix(arg, "--stdio=") || arg == "--listen" || strings.HasPrefix(arg, "--listen=") {
			return fmt.Errorf("Codex argument %q conflicts with cdrun's private stdio transport", arg)
		}
	}
	return nil
}
