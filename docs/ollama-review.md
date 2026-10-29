# Ollama PR review pilot

This repository uses PR-Agent with Ollama Cloud's
`deepseek-v4.1-flash:1017` checkpoint, with `low` thinking. Reviews appear in
the **Actions job summary**. The workflow does not post comments, approve PRs,
edit files or execute code from the PR.

## Enable and request a review

1. Merge the workflow and `.github/ollama-review/` runner into the default branch.
2. Add your own Ollama API key as the repository Actions secret `OLLAMA_API_KEY`.
   Do not put the key in a file or PR comment. The key must have cloud access.
3. Comment exactly `/review` on a PR, or choose **Actions → Ollama review →
   Run workflow** and enter its number. The requesting account must currently
   have write, maintain or admin permission. Results are under the run's summary.

Manual requests can review fork PRs safely: the trusted default-branch runner
only retrieves source as data. Do not enable GitHub's option to send secrets to
fork workflows. Ordinary contributors cannot spend the model budget by commenting.

For automatic reviews, set repository variable `OLLAMA_REVIEW_AUTO` to `true`.
This enables new/updated non-draft PRs from the same repository. It remains off
when the variable is absent. To disable all model calls, remove the secret or
disable the workflow in GitHub Actions.

## Request through the reviewer picker

GitHub's reviewer picker accepts people and teams; `github-actions[bot]` is not
a selectable reviewer. An ordinary machine-user account with repository access
can be used, or an existing collaborator can opt in. Set repository variable
`OLLAMA_REVIEW_REQUEST_LOGIN` to that account's login. A maintainer selecting it
in the picker triggers the same summary-only review. Other review requests do
not trigger this bot. Leave the variable absent to disable this trigger.

The selected account still receives GitHub's normal review request. This
workflow neither impersonates the account nor completes its human review.
There is no need for a personal access token or GitHub App for `/review`.

## Boundaries and limits

- The GitHub token has only contents/PR read permissions. The model key is
  supplied only to the analysis step, after a separate authorization job.
- Both jobs check out only trusted default-branch runner code. No PR checkout,
  tests, dependency hooks, MCP servers, shell tools or arbitrary comment arguments
  are executed. Only the fixed PR-Agent container performs analysis.
- Runtime: ten minutes for the analysis job; one model request, no retry,
  fallback model or chunking. The provider request times out after four minutes;
  the container has a seven-minute outer timeout. Authorized requests for the
  same PR cancel previous analysis. Unauthorized comments cannot cancel it.
  A small entry point around PR-Agent rejects extra inference requests and checks
  the exact endpoint, checkpoint, thinking setting and output cap before sending.
- Input: at most 100 changed files and 96 KiB of eligible patches; PR-Agent has
  a 48k token request budget and 16,384 output-token limit. Generated/vendor,
  common binary, lock and secret-file patterns are excluded. Extra context is
  PR-Agent's bounded hunk expansion, not repository-wide exploration. Arbitrary
  repository settings, AGENTS.md and linked ticket context are not loaded.
- The allowance is $2 per run. Admission conservatively prices a full 1,048,576
  input tokens plus the output limit at the recorded $0.30/$1.20 per-million
  list prices (about $0.3343). Usage is priced without cache discounts. This is
  not a provider-enforced billing limit: review pricing before activation and
  reconcile with the Ollama dashboard. Do not enable the pilot if its current
  pricing or output-limit behavior does not satisfy your budget.
- Missing source patches, missing metering/coverage, malformed output, model
  failures and output-cap hits are incomplete reviews. Coverage omissions remain
  visible. A PR whose base/head changes during review has its result discarded.
- At most three findings survive validation. Identical findings are collapsed.
  Locations must be visible in the captured right-side diff; other findings are
  shown only as unanchored summary items. No finding is proof of a defect.
- These are advisory summaries, not a required merge check. No failure is
  converted into an approval or a claim of correctness.
- Prompts and code go to Ollama Cloud. The workflow keeps review text in the
  Actions summary; raw engine output stays in the ephemeral runner. It does not
  upload prompt logs, reasoning traces or additional artifacts.

## Validate before broader use

Run the local contract tests without network or model calls:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s .github/ollama-review -v
```

`probe_engine.py` exercises the real pinned PR-Agent parser and request path
with a mocked completion. It uses no real credentials or inference. Run it in
the pinned container with `.github/ollama-review` mounted at `/probe`, and, for
a fully offline run, a pre-populated tiktoken cache mounted at `/tokenizer` with
`TIKTOKEN_CACHE_DIR=/tokenizer`. Its `o200k_base.tiktoken` vocabulary has SHA-256
`446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d`.

The pinned PR-Agent 0.49.0 image is selected by immutable multi-platform digest;
the runner explicitly selects Linux amd64, matching `ubuntu-24.04`.
The checkout action is also SHA-pinned. Update these pins deliberately.

Test one small known-bug PR and its corrected version, a clean change, then a
large/generated change. Confirm the actual model, thinking parameter, token
limit, request count and dashboard cost. Test a non-maintainer `/review`, a
fork requested by a maintainer, a new push during analysis and invalid output.
Do not enable PR comments until false positives, coverage and costs are
acceptable. Adding a publisher requires its own write-permission job and a
fresh-head check, not granting this analyzer write access.

References: [PR-Agent](https://github.com/The-PR-Agent/pr-agent),
[Ollama Cloud](https://docs.ollama.com/cloud),
[thinking controls](https://docs.ollama.com/capabilities/thinking),
[GitHub trigger security](https://docs.github.com/en/actions/reference/security/securely-using-pull_request_target).
GitHub's default public-repository `pull_request_target` event policy may require
an explicit allowance; `/review` and manual dispatch do not require that trigger.
