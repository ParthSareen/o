# Ollama PR review

An advisory GitHub Actions workflow using Pi and Ollama Cloud. A maintainer
requests a review with `/review`; the bot acknowledges it, explores relevant
repository source, and publishes findings on the PR. It never approves a PR or
requests changes. It is a review of changes, not a nightly repository scan,
penetration test, or test runner.

Two sibling reviewers are included:

| PR command | Input and behavior |
| --- | --- |
| `/review` | Diff and nearby source, then read-only repository exploration as needed. |
| `/review-diff` | Diff and nearby source only; one model call with just the submission tool, no repository exploration. |

The diff-only entry point and prompts live in `.github/ollama-review-diff/`.
It shares authorization, source collection, budget checks, output validation
and publishing with `.github/ollama-review/`. Each reviewer has its own status
comment, deduplication markers and concurrency group. Requesting both starts
two separately budgeted reviews; one does not cancel the other.

## Install a reviewed bundle

A repository maintainer needs permission to add workflows, an Ollama Cloud API
key, and approval to send the selected repository's source to Ollama Cloud.
Use one approved test repository first.

1. Obtain the reviewed ZIP and its SHA-256 from the person delivering it.
   Verify the ZIP's hash, extract it, and inspect `bundle.json` and the files.
2. Preview installation into a local working branch:
   ```sh
   python3 install.py --target /path/to/repository
   ```
3. After reviewing the preview, add `--apply`. The installer creates missing
   files, accepts identical files, and refuses conflicting files or symlinks.
   It does not commit, push, call a model, or change GitHub settings. Upgrades
   that differ require a manual diff; there is no force-overwrite option.
4. Review `.github/ollama-review/settings.json`:
   ```json
   {"profile": "general", "budget_usd": 5}
   ```
   `general` combines concrete correctness bugs with an explicit security pass.
   `security` reports only findings with demonstrated security impact. Both
   use the same pinned model. The diff-only reviewer has its own
   `.github/ollama-review-diff/settings.json` with the same options. Choose a positive budget
   up to $5; this is an estimated per-run allowance, not an account billing cap.
5. Add `OLLAMA_API_KEY` as a repository Actions secret. Never place a key in
   the settings file. Review and merge the installation through your normal
   process, then comment exactly `/review` on a small test PR.

Python 3, Git and a recent Node runtime are needed for local validation. GitHub's
Ubuntu runner installs pinned Node `24.22.2` and locked Pi packages itself.
Your organization must allow the pinned checkout/setup-node actions and
`pull-requests: write` for the feedback jobs. Use a dedicated CI key and the
available account controls; this workflow does not provision users or enforce
organization-wide budgets.

Expected result: one bot-owned comment progresses from Queued to Running to
Complete, or gives an explicit failure. Findings with valid locations also
appear inline. The comment and Actions summary show requests, repository-tool
calls, files read and estimated cost. Input usage is cumulative across requests;
the largest individual prompt is reported separately. Empty findings do not
establish safety.

## Triggers

`/review` and Actions → Ollama review → Run workflow are enabled.
For the diff-only sibling, use `/review-diff` or Actions → Ollama diff review →
Run workflow. It is manual-only and does not respond to reviewer-picker or
automatic PR events. The actor
must currently have repository write, maintain or admin permission. Untrusted
commenters cannot trigger spending or cancel a maintainer's running review.

Post the command on its own; surrounding spaces and trailing newlines are
accepted. The workflow first filters candidate comments, then Python checks
the exact trimmed command and current permission before starting a review.
Both workflows receive comment events, so the nonmatching workflow appears
as Skipped. That does not mean the requested review was skipped.

Automatic reviews are off. Set `OLLAMA_REVIEW_AUTO=true` only after the trial
is accepted; this enables eligible non-draft, same-repository PR events.
Forks remain manual. A maintainer must approve sending fork content to Cloud;
fork code is fetched as data and never executed.

GitHub's reviewer picker cannot select `github-actions[bot]`. Optionally set
`OLLAMA_REVIEW_REQUEST_LOGIN` to a real collaborator/machine-account login.
A maintainer requesting that account triggers the workflow, but the workflow
does not impersonate it or complete its human review.

## How it runs and what is protected

1. A read-only authorization job captures the open PR's base/head and changed
   files. All runner code comes from the trusted default branch.
2. A separate job posts the acknowledgment and running status.
3. Both analyzers collect the diff and nearby source. The repository reviewer then fetches an inert bare
   Git snapshot pinned to the head. There is no PR working-tree checkout,
   build, lifecycle script, shell tool, MCP server or execution of PR code.
4. In repository mode, Pi can `list_files`, `search_code` and `read_file`, including unchanged
   callers, implementations and tests. Tool results feed later model turns.
   Files and repository instructions are untrusted evidence, not instructions
   to execute. The Node process gets the model key and no GitHub token.
   In diff-only mode, no repository is fetched and only `submit_review` is
   registered. It uses the injected diff and bounded surrounding source;
   missing callers or definitions remain a limitation.
5. Both modes finish by calling `submit_review` with structured findings.
   The tool checks argument shape and changed-file paths, records the result
   locally and ends the agent without another model call. It does not publish
   directly. Plain-text or JSON final messages do not complete a review.
   The runner validates the submitted findings, usage and visible right-side
   inline ranges. It discards results if the base/head changed.
6. A publisher with PR write permission and no model key checks revisions
   again and publishes COMMENT feedback. Identical findings at an active
   location are deduplicated; different bugs on the same line remain distinct.
   Superseded runs cannot overwrite newer status. Prior reviews are used for
   publication deduplication, not as model context.

The workflow's least-privilege split follows
[GitHub's guidance](https://docs.github.com/en/actions/reference/security/secure-use).
Initial source and repository tools read only regular Git blobs, so symlinks
cannot bypass file exclusions. Generated/vendor, binary/lock and common
secret-file patterns are excluded. **These filters do not redact credentials
embedded in ordinary source.** Approve data access before installing.
Source is sent to Ollama Cloud; findings and file paths are retained in the PR
and Actions. The workflow does not upload raw prompts or reasoning traces as
artifacts. Review the provider's data terms with the customer separately.

## Limits and failure behavior

- At most 100 changed files, 96 KiB of eligible patches, 48 KiB of initial nearby
  source and 256 KiB of initial serialized input. Tools can inspect unchanged
  source: index up to 8 MiB, regular text blobs up to 256 KiB each, 200-line
  reads, 40-match searches and 100-path listings, with 2 MiB total tool output.
  Exclusions and missing initial context are reported. Missing context is not
  evidence that a guard does not exist.
- The analysis job has a ten-minute limit; inference has an eight-minute
  timeout. New authorized requests for the same PR cancel older analysis.
  There are no transport retries or fallback models.
  The acknowledgment job has five minutes for startup, status updates and
  GitHub runner cleanup. If that job fails or times out, analysis is skipped;
  check the failed dependency when diagnosing a skipped review job.
- Repository mode compacts older tool payloads when the serialized message
  history exceeds 256 KiB. It retains the original diff, assistant messages,
  tool-call IDs and eight most recent tool results. Older payloads are replaced
  with explicit notices so the model can re-read their source if needed. This
  is deterministic history reduction, not an extra summarization model call;
  it does not reset metered usage. Diff-only mode does not compact.
- `deepseek-v4.1-flash:1017`, low thinking, 1,048,576 context tokens and a separate
  393,216-token output maximum are pinned. Every model turn must report usage.
  The next output limit is the minimum of the provider maximum, remaining
  context and remaining dollar allowance after a conservative input reserve.
- Cost uses uncached list rates of $0.30/M input and $1.20/M output. Current
  limits/rates are in [Ollama's metadata](https://ollama.com/api/experimental/model-recommendations)
  and [model page](https://ollama.com/library/deepseek-v4.1-flash:1017-cloud).
  This is not a provider-enforced billing cap. Failed or unmetered requests may
  incur charges; reconcile actual billing and set account controls separately.
  When the provider reports cached input, an additional estimate uses $0.006/M
  for that subset. The conservative budget guard still charges all input at
  the uncached rate. Prompt totals include replayed history on every request;
  a cumulative total above the model window does not imply context overflow.
- Truncation, missing metering, invalid or absent submissions are incomplete
  reviews. In repository mode, a rejected submission can be corrected through
  the ordinary tool loop within the same budget and deadline. Diff-only mode
  remains one call. There is no free-text JSON parser or format-repair turn.
  Safe errors retain known usage without publishing raw provider
  bodies or reasoning. There is no finding-count quota. Very large PR comments
  are clipped with a link to the full Actions summary.

## Validate without spending

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s .github/ollama-review -v
npm ci --prefix .github/ollama-review --ignore-scripts --no-audit --no-fund
npm test --prefix .github/ollama-review
```

Tests use synthetic data and a mocked model transport. They exercise real Pi
serialization/tool continuation and local Git reads, authorization, stale heads,
reruns, invalid responses, spend limits and publishing behavior. No model call
or GitHub publication is made. `npm ci` downloads locked dependencies; it does
not run their lifecycle scripts.

`evals/` contains a known authorization defect, its fix through an unchanged
helper, and a clean control. The tests prove the fixtures behave as labeled.
They **do not prove the model detects the defect**. For a separately approved
model trial, make three synthetic PRs using each case's base/head `api.py` and
shared `storage.py`. Have a reviewer adjudicate the returned findings and fill
in a copy of `evals/labels.csv`, then run:

```sh
python3 .github/ollama-review/evaluate.py --labels /path/to/completed-labels.csv
```

Blank, incomplete or unsupported results cannot pass. Evaluate each selected
profile separately. No synthetic result is a substitute for reviewing a real
customer PR or for an independent security review of this workflow.

## Trial finish line

Before use, name a customer operator and reviewer, approve one repository and
Cloud data access, select the combined/security profile, and agree budget,
acceptable noise and an actual-billing check. Keep automatic review off.

With explicit approval for paid runs: evaluate the known defect/fix/clean
controls, then one small real PR. Verify fresh-head behavior, a rerun without
duplicate findings, and failure/cancellation status. Record confirmed bugs,
unsupported findings, time, estimated and actual cost, and reviewer effort.

The customer must then disposition the findings and decide whether to keep
using it. A passing workflow, provisioned credits or this package alone does
not establish adoption, security effectiveness or cost savings.

## Build a bundle locally

From this source repository:

```sh
python3 .github/ollama-review/package.py /tmp/ollama-review.zip --profile general
```

Choose `--profile security` for focused security review. This packages saved
files, including local uncommitted changes. `bundle.json` records a base Git
commit and payload hashes; the base commit alone does not identify a candidate
containing local changes. The command prints the ZIP SHA-256 and refuses to
replace an existing ZIP. Deliver only a reviewed, versioned candidate.
