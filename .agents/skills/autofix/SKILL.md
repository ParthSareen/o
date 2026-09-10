---
name: autofix
description: Iterate on an implementation with independent kimi-k3 code reviews run through `o` headless until verified findings are resolved. Use when the user explicitly asks for an autofix or review-fix loop.
---

# Autofix (o + kimi-k3)

Use this only when the user explicitly requests an autofix loop. It is a controlled review-and-repair cycle, not permission to broaden scope or publish changes.

The reviewer is `o` itself running `kimi-k3:cloud` in headless mode with all tools approved. The main thread (the agent driving this skill) launches it as a background process and polls it to confirm it is producing real review output, not hanging or erroring silently.

## Loop

1. Establish the intended behavior, scope, validation command(s), and the current branch/worktree. Preserve unrelated dirty changes.
2. Launch an independent review using `o` headless with full perms:

   ```sh
   o --headless --allow-all-tools kimi-k3:cloud "<review prompt>" > review.out 2> review.err
   ```

   The review prompt must be self-contained: state the goal (review the current diff and relevant surrounding code), the required output format (findings with severity, exact file/line references, and a merge verdict), and the constraints (do not modify code, do not post externally, do not commit or push). Pipe the diff in via stdin or embed paths.

   Run it in the background (`bash` with `background=true`) and poll the log/output files from the main thread:
   - Confirm the process started and is not immediately exited with an error (check `review.err` and the exit record).
   - Confirm real review content is appearing in `review.out` (not empty, not a repeat of the prompt, not a tool-denial message).
   - If `o` exits 1, read `review.err`: exit 1 means either an error or a tool was denied — both are blockers.
   - Wait for exit 0 and a complete, non-empty `review.out` before treating the review as done.
3. Turn the review into a small local finding ledger: resolved, rejected with evidence, or needs decision. Do not blindly apply every suggestion. Verify every finding against the code and intended behavior.
4. Fix verified findings in the existing implementation worktree. Keep changes narrow; run the relevant focused tests or reproduction after each meaningful fix.
5. Start a fresh `o` headless review on the updated head. Repeat until the review has no merge-blocking findings and the required validation passes.

## Polling the reviewer

The main thread owns the loop. When it launches the `o` review it must verify the reviewer is actually working, not just fire-and-forget:

- Launch with `background=true`. Note the task ID, PID, and log path.
- Poll periodically: `tail` the output/error logs, check the `.exit` file for completion and code.
- A clean run looks like: process alive for a reasonable span, `review.err` shows thinking/tool activity (or is empty), `review.out` grows with structured findings, final exit 0.
- A broken run looks like: immediate exit 1, `review.err` contains a tool denial or API error, `review.out` is empty or echoes the prompt. In that case, fix the invocation (prompt clarity, model availability, perms) and relaunch — do not proceed on a silent failure.
- Do not treat "the process exited 0 with an empty answer" as success. Require a substantive review.

## Escalation

Pause and ask the user before continuing when a finding is ambiguous or any resolution would:

- change product behavior, API semantics, permissions, data handling, or compatibility;
- require a tradeoff the user has not chosen;
- conflict with existing requirements, tests, or review feedback; or
- need credentials, production access, destructive cleanup, or an external write.

State the finding, the competing options, the evidence, and your recommended choice. Resume the same loop after the user decides.

## Boundaries

- Treat reviewer output as evidence, not authority. Verify every finding against the code and intended behavior.
- Do not dismiss a finding just because a test passes; add or adjust focused coverage when the issue is real and the test gap matters.
- Do not stop after a reviewer is merely silent: require an explicit clean verdict and report what was validated.
- Do not commit, push, create a PR, post reviews, or merge unless the user explicitly authorizes it.
- The `o` reviewer runs with `--allow-all-tools`, so it can read files and run bash to inspect the diff. It must not be instructed to write or mutate code during the review step.
- Clean up review scratch files (`review.out`, `review.err`, `.exit`) after each round unless the user asks to keep them.

## Final Report

Report the reviewer rounds, resolved findings, any user decisions, focused validation, remaining non-blocking risks, and the exact next action. Keep it concise and do not include chain-of-thought or raw tool output.
