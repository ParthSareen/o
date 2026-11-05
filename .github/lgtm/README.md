# LGTM review runner

Vendored from [ParthSareen/lgtm](https://github.com/ParthSareen/lgtm) at
`3904366b6cec9a9fde513cb0e1bb11827051cede`.

Edit `settings.json` for the shared prompt, per-mode model, thinking, profile,
and budget. The repository and diff modes both use the general profile and a
$5 budget. Request them with `/review` and `/review-diff` on a pull request.

Run offline checks with:

```sh
npm ci --prefix .github/lgtm --ignore-scripts --no-audit --no-fund
npm test --prefix .github/lgtm
```

See [the review guide](../../docs/ollama-review.md) for workflow behavior.
