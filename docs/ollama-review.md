# Ollama PR review pilot

This repository uses Pi's agent core and OpenAI-compatible transport with
Ollama Cloud's `deepseek-v4.1-flash:1017` checkpoint and `low` thinking.
Reviews appear in a persistent **PR comment**, with inline findings where their
locations are valid, and in the **Actions job summary**. No tools, extensions, sessions,
repository instructions or executable PR code are loaded by the agent.
The workflow never approves PRs or requests changes.

## Request a review

Add your Ollama API key as the repository Actions secret `OLLAMA_API_KEY`.
Then comment exactly `/review` on a PR, or choose **Actions → Ollama review →
Run workflow** and enter its number. The requester must currently have write,
maintain or admin permission. The bot acknowledges the request, updates the
comment when the pipeline starts, then posts findings or a clear terminal error.
The same comment is reused on subsequent requests.

Manual requests can review forks: the default-branch runner retrieves source
through GitHub's API as data. Do not enable sending secrets to fork workflows.
Ordinary contributors cannot spend the model budget by commenting.

Set repository variable `OLLAMA_REVIEW_AUTO=true` for automatic reviews of new
or updated non-draft PRs from this repository. It is off when absent.

## Reviewer picker

GitHub's picker accepts people and teams; `github-actions[bot]` is not selectable.
To use an existing collaborator or machine account as the trigger, set
`OLLAMA_REVIEW_REQUEST_LOGIN` to its login. A maintainer selecting that account
requests the same review. Other review requests are ignored.
The account still receives GitHub's normal review request; this workflow does
not impersonate it or complete its human review. Leave the variable absent to
disable this trigger. `/review` needs no extra account, PAT or GitHub App.

## How the runner works

1. **Authorize:** a GitHub-hosted Ubuntu job loads the runner from the default
   branch, checks the requester's current repository permission, and captures
   the open PR's base/head revisions and eligible changed-file list.
2. **Acknowledge:** a separate job creates or updates one bot-owned PR comment
   to say the request is queued, then that the pipeline has started.
3. **Collect context:** the analysis job fetches patches and nearby source via
   the GitHub API at the captured revisions. It sends the prompt plus this
   packet, not a checkout of the PR or the whole repository.
4. **Analyze:** Python starts the Node Pi adapter. Pi sends one streaming
   request to Ollama Cloud using the checked-in [prompt](../.github/ollama-review/prompt.md)
   as the system message and the JSON code packet as the user message.
   Pi has no tools and cannot explore files or run commands. This is a single
   review inference, not an interactive coding session. The Node process gets
   the model key but no GitHub token.
5. **Validate:** the runner requires a complete JSON answer and token usage,
   removes exact duplicate findings, checks inline locations against the diff,
   and verifies the PR still has the captured base/head. It writes the Actions
   summary and passes the structured result to the publisher.
6. **Publish:** the publisher rechecks the revisions and locations, posts new
   inline findings in a COMMENT review, and changes the persistent PR comment
   to Complete or an explicit failure/cancellation/superseded status. It has PR
   write permission and no model key. Existing bot comments are read only to
   avoid duplicate publication; previous reviews are not model context.

## Review prompt

The full system instruction is versioned in
[`.github/ollama-review/prompt.md`](../.github/ollama-review/prompt.md).
It asks for every distinct supported bug, ordered by impact, without a finding
quota. Each candidate must have a concrete trigger, observable consequence and
code evidence. The reviewer checks counterevidence and existing guards before
reporting it, and omits claims that require missing context. Multiple symptoms
of one root cause become one finding; unrelated causes stay separate.

An explicit security pass traces attacker-controlled inputs through trust
boundaries to sensitive operations. It covers relevant authorization and tenant
isolation, injection, file/network access, secret exposure, TLS/crypto changes,
resource exhaustion, workflow abuse and CI/agent permissions. Security findings
start with `Security:` and must describe attacker prerequisites, a reachable
path, the ineffective control and its impact. A suspicious pattern alone does
not qualify as a finding.

The design follows [Anthropic's context-engineering guidance](https://www.anthropic.com/research/effective-context-engineering-for-ai-agents)
on clear, structured instructions and relevant context, and
[OWASP's secure-code-review guidance](https://cheatsheetseries.owasp.org/cheatsheets/Secure_Code_Review_Cheat_Sheet.html)
on data flow, changed trust boundaries and business logic. These are design
inputs, not evidence that this prompt improves this DeepSeek checkpoint.
Before claiming an improvement, compare prompts on the same known-bug,
corrected and clean PR snapshots; score distinct confirmed bugs, false positives,
security findings, latency and token use. Include code containing misleading
instructions and cases where a visible guard prevents an apparent bug.

## Boundaries and limits

- Authorization is a separate job, before the model key is supplied. The
  analysis job has only contents/PR read permissions, and its GitHub token is
  never passed to Pi. Separate feedback/publisher jobs receive PR write access
  but no model key. All comment writes are serialized per PR.
  Unauthorized comments cannot cancel a maintainer's running review.
- Only trusted default-branch runner code is checked out. There is no PR
  checkout, build, dependency hook, test execution, MCP server or shell tool.
  Pi dependencies and their integrity hashes are locked; `npm ci` disables
  lifecycle scripts. Node and GitHub actions are pinned.
- One model request, no retries, fallback models or continuation turns.
  The analysis job has a ten-minute deadline; inference has an eight-minute
  timeout and a nine-minute process limit. A new authorized request for the
  same PR cancels the old one.
- At most 100 changed files, 96 KiB of eligible patches, 48 KiB of selective
  surrounding code, and 256 KiB of total serialized input. Context is numbered
  source around changed hunks, fetched at the captured head (base for deletions).
  Generated/vendor files, common binary/lock files and secret-file patterns are
  excluded. Exclusions and unavailable context are listed in the summary.
- Calibration mode currently omits the client-side output-token cap, at the
  repository owner's request. The provider's own limits still apply. The
  allowance is $2 per run. Admission conservatively reserves 1,048,576 input
  and output tokens each at the recorded $0.30/$1.20 per-million list prices
  (about $1.5729); input/output share the model context in practice.
  Usage is priced without cache discounts. This is an estimate, not a
  provider-enforced billing limit; review current prices and dashboard billing.
- HTTP failures, truncated streams, missing metering and invalid JSON are
  incomplete reviews. Safe diagnostics name the failure without exposing raw
  provider bodies, prompts or reasoning. Changed base/head revisions cause the
  result to be discarded.
- There is no finding-count cap in the prompt, Pi adapter, validator or
  publisher. Runtime, context and transport-size limits still apply. Exact
  duplicate findings are collapsed. Inline locations are validated against visible right-side diff
  lines; unanchored findings remain in the main PR comment. Existing bot
  findings are deduplicated. A superseded run cannot overwrite a newer status.
  If a PR comment exceeds GitHub's size allowance, it links to the full Actions
  summary rather than silently treating the clipped text as the entire review.
- Checks are advisory. Empty findings do not prove correctness. Code is sent
  to Ollama Cloud; findings are published on the PR and retained in Actions. No raw prompt,
  reasoning trace or extra artifact is uploaded.

## Validate changes

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s .github/ollama-review -v
npm ci --prefix .github/ollama-review --ignore-scripts --no-audit --no-fund
npm test --prefix .github/ollama-review
```

The Node tests use Pi's real agent loop and HTTP serialization with a local
mock transport. They check the exact model/endpoint/thinking and absence of a client output cap,
stream parsing, usage accounting, no retries, and refusal of tool requests.
Regression checks also pass more than three distinct findings through Pi,
normalization, the Actions summary and PR publication.
Low thinking is explicit. A later measured output cap should be reintroduced
after calibration; tests currently assert that no client output cap is sent.
They make no network or paid inference calls.

Before broader use, compare a known-bug PR with its corrected version, a clean
change, and a large/generated change. Confirm actual request count, findings,
latency and dashboard cost. Exercise a rerun, cancellation and a head change:
old results must not replace the current status, and duplicate inline findings
must not be posted. The analyzer remains read-only.

References: [Pi](https://github.com/earendil-works/pi),
[Ollama Cloud](https://docs.ollama.com/cloud),
[thinking controls](https://docs.ollama.com/capabilities/thinking),
[GitHub trigger security](https://docs.github.com/en/actions/reference/security/securely-using-pull_request_target).
GitHub's public-repository `pull_request_target` policy may require an explicit
allowance; `/review` and manual dispatch do not require that trigger.
