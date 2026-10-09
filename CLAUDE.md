# CLAUDE.md

Project rules live in AGENTS.md and are imported here:

@AGENTS.md

Claude-specific workflow:

- Main agent (Opus) reads the real code, makes architecture decisions, splits work and defines acceptance criteria. Simple tasks: do them directly.
- Delegate only bounded, worthwhile implementation work to `sonnet-implementer`, with a complete contract: goal, files/scope, constraints, acceptance criteria, verification to run.
- Sonnet returns a change summary, verification evidence and risks.
- Opus personally reviews the final diff, shared call paths, edge cases and test results; send targeted fixes back to Sonnet only when needed. End with PASS, FAIL or BLOCKED — no repeated review loops or duplicate execution.
