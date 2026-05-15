# Push

## PR body
Disable interactive Codex approval prompts while keeping the existing sandbox boundary.

`internal/agent/codex/codex.go` now passes `--ask-for-approval never` for interactive Codex sessions instead of `on-request`, so MoE-managed interactive runs no longer block on human approval prompts. The write boundary still comes from the sandbox plus `--add-dir`; this change only removes the approval dialogue.

`internal/agent/codex/codex_test.go` was updated to assert the new interactive approval flag and to keep checking that the sandbox remains `workspace-write`.

## Ship readiness
The change is localized to Codex argument construction and is covered by unit tests that inspect the generated command line. Those tests confirm both the new `--ask-for-approval never` behavior and the preserved sandbox boundary.

## Conflicts surfaced
None.
