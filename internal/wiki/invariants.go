package wiki

import "fmt"

// AssertModeInvariants is the guardrail the engine runs pre-finalize
// (and may run mid-session via a hook in a future revision). For
// open-schema wikis it's a no-op — the agent is permitted to evolve
// the doc set freely. For closed-schema wikis (the twin) it will
// refuse a finalize that would add or remove topic docs the operator
// hasn't authorized.
//
// Phase 1 ships the open-schema half; the closed-schema branch is a
// stub returning a "not yet implemented" error so a misconfigured
// closed wiki fails loudly instead of silently behaving like an open
// one.
func AssertModeInvariants(cfg Config) error {
	switch cfg.Mode {
	case Open:
		return nil
	case Closed:
		return fmt.Errorf("wiki: closed-schema invariants not implemented (phase 2)")
	default:
		return fmt.Errorf("wiki: unknown mode %d", cfg.Mode)
	}
}
