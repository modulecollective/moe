package cli

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

// installGitTrace installs git.Hook when MOE_GIT_TRACE=1, writing one
// line per git invocation to stderr. Strict =1 — anything else is off.
// The mutex serialises the writes; without it, concurrent git calls
// could interleave Fprintf output. Production callers invoke this once
// at startup from cli.Run, before any command dispatches.
func installGitTrace() {
	if os.Getenv("MOE_GIT_TRACE") != "1" {
		return
	}
	var mu sync.Mutex
	git.Hook = func(dir string, args []string, dur time.Duration, err error) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(os.Stderr, "git-trace dir=%q args=%q dur=%s err=%v\n",
			dir, args, dur, err)
	}
}
