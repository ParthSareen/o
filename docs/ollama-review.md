# Ollama PR review pilot

This repository uses Pi's agent core and OpenAI-compatible transport with
Ollama Cloud's `deepseek-v4.1-flash:1017` checkpoint and `low` thinking.
Reviews appear in the **Actions job summary**. No tools, extensions, sessions,
repository instructions or executable PR code are loaded by the agent.
The workflow does not post comments or approve PRs.

## Request a review

Add your Ollama API key as the repository Actions secret `OLLAMA_API_KEY`.
Then comment exactly `/review` on a PR, or choose **Actions → Ollama review →
Run workflow** and enter its number. The requester must currently have write,
maintain or admin permission. Results appear in the run's summary.

Manual requests can review forks: the default-branch runner retrieves source
through GitHub's API as data. Do not enable sending secrets to fork workflows.
Ordinary contributors cannot spend the model budget by commenting.

Set repository variable `OLLAMA_REVIEW_AUTO=true` for automatic reviews of new
or updated non-draft PRs from this repository. It is off when absent.

## Reviewer picker

GitHub's picker accepts people and teams; `github-actions[bot]` is not selectable.
To use an existing collaborator or machine account as the trigger, set
`OLLAMA_REVIEW_REQUEST_LOGIN` to its login. A maintainer selecting that account
requests the same summary-only review. Other review requests are ignored.
The account still receives GitHub's normal review request; this workflow does
not impersonate it or complete its human review. Leave the variable absent to
disable this trigger. `/review` needs no extra account, PAT or GitHub App.

## Boundaries and limits

- Authorization is a separate job, before the model key is supplied. The
  GitHub token has only contents/PR read permissions and is never passed to Pi.
  Unauthorized comments cannot cancel a maintainer's running review.
- Only trusted default-branch runner code is checked out. There is no PR
  checkout, build, dependency hook, test execution, MCP server or shell tool.
  Pi dependencies and their integrity hashes are locked; `npm ci` disables
  lifecycle scripts. Node and GitHub actions are pinned.
- One model request, no retries, fallback models or continuation turns.
  The analysis job has a ten-minute deadline; inference has a four-minute
  timeout and a five-minute process limit. A new authorized request for the
  same PR cancels the old one.
- At most 100 changed files, 96 KiB of eligible patches, 48 KiB of selective
  surrounding code, and 256 KiB of total serialized input. Context is numbered
  source around changed hunks, fetched at the captured head (base for deletions).
  Generated/vendor files, common binary/lock files and secret-file patterns are
  excluded. Exclusions and unavailable context are listed in the summary.
- The output limit is 16,384 tokens, including reasoning. The allowance is $2
  per run. Admission reserves a full 1,048,576 input tokens plus the output
  limit at the recorded $0.30/$1.20 per-million list prices (about $0.3342).
  Usage is priced without cache discounts. This is an estimate, not a
  provider-enforced billing limit; review current prices and dashboard billing.
- HTTP failures, truncated streams, missing metering and invalid JSON are
  incomplete reviews. Safe diagnostics name the failure without exposing raw
  provider bodies, prompts or reasoning. Changed base/head revisions cause the
  result to be discarded.
- The model may return up to three concrete bug findings. Repeated findings
  are collapsed. Inline locations are validated against visible right-side diff
  lines; unanchored findings remain clearly marked summary items.
- Checks are advisory. Empty findings do not prove correctness. Code is sent
  to Ollama Cloud; only the review summary is retained in Actions. No raw prompt,
  reasoning trace or extra artifact is uploaded.

## Validate changes

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s .github/ollama-review -v
npm ci --prefix .github/ollama-review --ignore-scripts --no-audit --no-fund
npm test --prefix .github/ollama-review
```

The Node tests use Pi's real agent loop and HTTP serialization with a local
mock transport. They check the exact model/endpoint/thinking/output limit,
stream parsing, usage accounting, no retries, and refusal of tool requests.
They make no network or paid inference calls.

Before broader use, compare a known-bug PR with its corrected version, a clean
change, and a large/generated change. Confirm actual request count, findings,
latency and dashboard cost. Keep PR publication disabled until those results
are acceptable. A future publisher needs its own write-permission job and a
fresh-head check; the analyzer should remain read-only.

References: [Pi](https://github.com/earendil-works/pi),
[Ollama Cloud](https://docs.ollama.com/cloud),
[thinking controls](https://docs.ollama.com/capabilities/thinking),
[GitHub trigger security](https://docs.github.com/en/actions/reference/security/securely-using-pull_request_target).
GitHub's public-repository `pull_request_target` policy may require an explicit
allowance; `/review` and manual dispatch do not require that trigger.
