"""Pinned PR-Agent entry point with request and coverage accounting."""

import json
from pathlib import Path

from pr_agent import cli
from pr_agent.algo.ai_handlers import litellm_ai_handler
from pr_agent.git_providers.github_provider import GithubProvider


audit = {"requests": 0, "finish_reason": None, "files": []}
original_completion = litellm_ai_handler.acompletion
original_diff_files = GithubProvider.get_diff_files


async def completion(**kwargs):
    if audit["requests"]:
        raise RuntimeError("The one-request review budget has been consumed")
    if (kwargs.get("model") != "openai/deepseek-v4.1-flash:1017"
            or kwargs.get("api_base") != "https://ollama.com/v1"
            or kwargs.get("reasoning_effort") != "low"
            or kwargs.get("max_tokens") != 16384):
        raise RuntimeError("Unexpected model request configuration")
    audit["requests"] += 1
    kwargs["num_retries"] = 0
    response = await original_completion(**kwargs)
    audit["finish_reason"] = response.choices[0].finish_reason
    return response


def diff_files(self, *args, **kwargs):
    files = original_diff_files(self, *args, **kwargs)
    audit["files"] = sorted({file.filename for file in files})
    return files


litellm_ai_handler.acompletion = completion
GithubProvider.get_diff_files = diff_files


if __name__ == "__main__":
    try:
        raise SystemExit(cli.run())
    finally:
        Path("/output/audit.json").write_text(json.dumps(audit))
