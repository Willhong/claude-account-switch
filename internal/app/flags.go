package app

import (
	"flag"
	"fmt"
	"strings"
)

// parseFlags parses args and returns the positional ones.
//
// Go's flag package stops at the first non-flag argument, which would silently
// drop the flag in `cas remove 2 --yes`. This keeps parsing across positionals
// so flags work wherever the user puts them.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseNoArgs parses args for a command that takes no positional arguments.
func parseNoArgs(fs *flag.FlagSet, args []string) error {
	extra, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return fmt.Errorf("%s takes no arguments, got %s", fs.Name(), strings.Join(quoteAll(extra), " "))
	}
	return nil
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = fmt.Sprintf("%q", a)
	}
	return out
}
