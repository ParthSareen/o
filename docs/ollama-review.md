# Pull request reviews

LGTM reviews pull requests through Pi and Ollama Cloud. A maintainer can post
one of these commands as the whole PR comment:

| Command | Context |
| --- | --- |
| `/review` | Diff, nearby source, and read-only repository tools |
| `/review-diff` | Diff and nearby source in one model request |

Both commands use [.github/workflows/lgtm.yml](../.github/workflows/lgtm.yml).
Each run posts a status comment and updates it through queued, running, and
complete or incomplete. Supported findings appear in the summary and inline
when they have a valid diff location. Previous reviews are used for publishing
deduplication; they are not included in the model's context.

## Configuration

Edit [.github/lgtm/settings.json](../.github/lgtm/settings.json). The top-level
`prompt` contains the full shared instructions. Each mode can override `prompt`,
`model`, `thinking`, `profile`, and `budget_usd`; a null or omitted mode prompt
inherits the shared text. Both modes retain the general profile, low thinking,
and a $5 estimated budget per run. Model definitions include their context and
output limits, token prices, and supported thinking levels.

The runner accepts up to 100 changed files, 512 KiB of patches, 48 KiB of nearby
source, and 1 MiB of initial model input. Repository-tool history can compact
after 256 KiB. Analysis has an eight-minute deadline inside a ten-minute job.

The `OLLAMA_API_KEY` Actions secret supplies the model key. Automatic reviews
remain opt-in through `OLLAMA_REVIEW_AUTO`; fork reviews require a manual
request. `OLLAMA_REVIEW_REQUEST_LOGIN` optionally enables an actual collaborator
in GitHub's reviewer picker. Both modes can also be started through
**Actions → Ollama review → Run workflow**, choosing the PR and mode.

## Runner and checks

The installed source revision is recorded in
[.github/lgtm/README.md](../.github/lgtm/README.md). The workflow prepares the
request, analyzes it, and publishes the findings in separate jobs. Runner code
comes from the workflow commit. PR code is read as data in an inert bare Git
snapshot; it is never checked out or executed by the reviewer.

`LGTM tests` validates the installed runner without a model key or paid calls:

```sh
npm ci --prefix .github/lgtm --ignore-scripts --no-audit --no-fund
npm test --prefix .github/lgtm
```
