package cli

// reorderFlags moves flag-looking tokens ("-x", "--x", "--x=y") to the
// front of the slice, preserving original order within each group. The
// Go stdlib `flag` package stops parsing at the first non-flag token,
// so `moe sdlc new moe --from-idea=x` would otherwise treat
// `--from-idea=x` as a positional title. Commands that call
// fs.Parse(reorderFlags(args)) tolerate flags placed anywhere in the
// arg list.
//
// The `--` sentinel is respected: once seen, everything after it is
// treated as positional (the sentinel itself is preserved at the
// boundary between flags and positionals in the output).
func reorderFlags(args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	sawSentinel := false
	for _, a := range args {
		if sawSentinel {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			sawSentinel = true
			continue
		}
		if isFlagToken(a) {
			flags = append(flags, a)
		} else {
			positional = append(positional, a)
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
