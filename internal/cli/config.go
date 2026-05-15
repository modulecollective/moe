package cli

import (
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/modulecollective/moe/internal/config"
)

// `moe config` is the operator-local key/value surface that owns
// .moe/config.json. The keyspace is closed (see internal/config),
// today with just `default_agent` to provide a persistent default
// for the agent backend without re-exporting $MOE_AGENT into every
// shell rc. Not a Workflow — no canvas, no ladder, no dash — so
// register it as a plain CommandGroup.

func init() {
	g := NewCommandGroup("config", "operator-local config: list, get, set, unset (.moe/config.json)")
	g.Register(&Command{
		Name:    "list",
		Summary: "print every known key with its current value",
		Run:     runConfigList,
	})
	g.Register(&Command{
		Name:    "get",
		Summary: "print one value (empty if unset; exit 0 either way)",
		Run:     runConfigGet,
	})
	g.Register(&Command{
		Name:    "set",
		Summary: "set a value (validated per-key)",
		Run:     runConfigSet,
	})
	g.Register(&Command{
		Name:    "unset",
		Summary: "clear a value back to the fallthrough default",
		Run:     runConfigUnset,
	})
	RegisterGroup(g)
}

// configUnsetMarker is what `moe config list` prints for a key
// with no value on disk. Picked to be visibly non-empty so an
// operator skimming the output can tell "this is unset" from "this
// is set to the empty string." The `(unset)` token is not a valid
// value for any allowlisted key, so it can't shadow a real setting.
const configUnsetMarker = "(unset)"

func runConfigList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe config list") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	c, err := config.Read(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	// Walk the full allowlist (not just the populated keys on c) so
	// the operator always sees every available knob. config.Keys is
	// already sorted, so the order is stable across invocations —
	// grep-from-shell stays predictable.
	for _, k := range config.Keys() {
		v, _ := config.Get(c, k)
		if v == "" {
			v = configUnsetMarker
		}
		moePrintf(stdout, "%s = %s\n", k, v)
	}
	return 0
}

func runConfigGet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe config get <key>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	key := fs.Arg(0)
	if !config.Known(key) {
		moePrintf(stderr, "config get: unknown key %q (valid: %s)\n", key, strings.Join(config.Keys(), ", "))
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	c, err := config.Read(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	v, _ := config.Get(c, key)
	// Print exactly the value, newline-terminated. An unset key
	// prints an empty line and exits 0 — matches `git config --get`
	// convention. The design's open question 1 landed here; a future
	// switch to exit-1-on-unset is a one-line change.
	moePrintln(stdout, v)
	return 0
}

func runConfigSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe config set <key> <value>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	key, value := fs.Arg(0), fs.Arg(1)
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	c, err := config.Read(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := config.Set(&c, key, value); err != nil {
		if errors.Is(err, config.ErrUnknownKey) {
			moePrintf(stderr, "config set: %v\n", err)
			return 2
		}
		moePrintf(stderr, "config set %s: %v\n", key, err)
		return 1
	}
	if err := config.Write(root, c); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "%s = %s\n", key, value)
	return 0
}

func runConfigUnset(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config unset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe config unset <key>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	key := fs.Arg(0)
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	c, err := config.Read(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := config.Unset(&c, key); err != nil {
		moePrintf(stderr, "config unset: %v\n", err)
		return 2
	}
	if err := config.Write(root, c); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	moePrintf(stdout, "%s unset\n", key)
	return 0
}
