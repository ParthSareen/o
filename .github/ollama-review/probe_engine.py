"""Offline integration check, run inside the pinned PR-Agent container."""

import contextlib
import asyncio
import io
import json
import os
from pathlib import Path
import sys
import tempfile
from unittest.mock import patch

from review import MAX_OUTPUT_TOKENS, MODEL, engine_environment

os.environ.update(engine_environment("unused-test-token", "unused-test-key"))

from litellm import ModelResponse
from pr_agent import cli
import engine

requests = []


async def completion(**kwargs):
    requests.append(kwargs)
    return ModelResponse(
        model=MODEL,
        choices=[{"message": {"role": "assistant", "content": "review:\n  key_issues_to_review: []\n"},
                  "finish_reason": "stop"}],
        usage={"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
    )


with tempfile.TemporaryDirectory() as tmp:
    folder = Path(tmp)
    (folder / "change.diff").write_text(
        "diff --git a/example.py b/example.py\n--- a/example.py\n+++ b/example.py\n"
        "@@ -1 +1 @@\n-print('before')\n+print('after')\n"
    )
    sys.argv = ["pr-agent", "--diff-file", str(folder / "change.diff"),
                "--json-output", str(folder / "result.json"), "review"]
    with patch.object(engine, "original_completion", completion), contextlib.redirect_stdout(io.StringIO()):
        exit_code = cli.run()
    assert exit_code in (None, 0), exit_code
    result = json.loads((folder / "result.json").read_text())

assert len(requests) == 1, len(requests)
assert engine.audit["requests"] == 1
assert engine.audit["finish_reason"] == "stop"
request = requests[0]
assert request["model"] == f"openai/{MODEL}", request.get("model")
assert request["api_base"] == "https://ollama.com/v1", request.get("api_base")
assert request["reasoning_effort"] == "low", request.get("reasoning_effort")
assert request["max_tokens"] == MAX_OUTPUT_TOKENS, request.get("max_tokens")
assert result["review"]["key_issues_to_review"] == []
assert result["coverage"] == {"files_not_reviewed": [], "files_partially_reviewed": []}
assert result["usage"]["prompt_tokens"] == 100, result["usage"]
try:
    asyncio.run(engine.completion(**request))
except RuntimeError as error:
    assert "one-request" in str(error)
else:
    raise AssertionError("A second inference request was allowed")
print("Pinned engine: one mocked request; endpoint, model, thinking, output cap, JSON, coverage and usage verified.")
