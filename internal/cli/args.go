package cli

import (
	"flag"
	"strings"
)

// reorderFlags moves flag-looking tokens ("-x", "--x", "--x=y") to the
// front of the slice, preserving original order within each group. The
// Go stdlib `flag` package stops parsing at the first non-flag token,
// so `moe sdlc new moe --from-idea=x` would otherwise treat
// `--from-idea=x` as a positional title. Commands that call
// fs.Parse(reorderFlags(fs, args)) tolerate flags placed anywhere in
// the arg list.
//
// The FlagSet is consulted so that the space form `--flag value` keeps
// the value adjacent to its flag across the reorder. Without this, a
// non-bool flag in space form would be split apart and `flag.Parse`
// would silently consume an unrelated positional as the value.
//
// The `--` sentinel is respected: once seen, everything after it is
// treated as positional (the sentinel itself is preserved at the
// boundary between flags and positionals in the output).
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	sawSentinel := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if sawSentinel {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			sawSentinel = true
			continue
		}
		if !isFlagToken(a) {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// `--flag=value` is a single token — already paired.
		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		f := fs.Lookup(name)
		if f == nil || isBoolFlag(f) {
			// unknown or bool: don't speculatively swallow the next
			// token. fs.Parse will report unknown-flag errors itself.
			continue
		}
		// value-taking flag in space form: pair with next token.
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	out := make([]string, 0, len(args))
	out = append(out, flags...)
	if sawSentinel {
		out = append(out, "--")
	}
	out = append(out, positional...)
	return out
}

// isFlagToken reports whether s looks like a flag argument: starts
// with `-` and has at least one more character. Bare `-` is treated as
// positional (conventional stdin indicator), and `--` is handled by
// the caller as a sentinel.
func isFlagToken(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	if s == "--" {
		return false
	}
	return true
}

// isBoolFlag reports whether f is a boolean flag, by probing the
// stdlib's IsBoolFlag() interface contract that flag.BoolVar's value
// type implements.
func isBoolFlag(f *flag.Flag) bool {
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
		return b.IsBoolFlag()
	}
	return false
}
