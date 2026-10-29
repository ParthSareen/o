#!/usr/bin/env python3
"""Run a bounded Pi review of a GitHub pull request."""

import argparse
import base64
import fnmatch
import hashlib
import html
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


MODEL = "deepseek-v4.1-flash:1017"
MAX_FILES = 100
MAX_PATCH_BYTES = 96 * 1024
MODEL_CONTEXT_TOKENS = 1048576
MAX_CONTEXT_BYTES = 48 * 1024
BUDGET_USD = 5.0
# Admission estimate only. Provider billing is reconciled during the pilot.
INPUT_USD_PER_M = 0.30
OUTPUT_USD_PER_M = 1.20
EXCLUDES = [
    "vendor/**", "**/vendor/**", "node_modules/**", "**/node_modules/**",
    "*.lock", "go.sum", "*.pb.go", "*.generated.*", "*.min.js", "*.map",
    "*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.ico", "*.pdf",
    "*.zip", "*.gz", "*.woff*", "*.ttf", ".env*", "**/.env*", "*.pem", "*.key",
]
POLICY = Path(__file__).with_name("prompt.md").read_text(encoding="utf-8")


class ReviewError(Exception):
    def __init__(self, message, metrics=None):
        super().__init__(message)
        self.metrics = metrics or {}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ReviewError("GitHub unexpectedly redirected an authenticated request")


class GitHub:
    def __init__(self, repository, token):
        if not re.fullmatch(r"[\w.-]+/[\w.-]+", repository, flags=re.ASCII):
            raise ReviewError("Invalid repository")
        if not token:
            raise ReviewError("GITHUB_TOKEN is required")
        self.repository = repository
        self.token = token
        self.opener = urllib.request.build_opener(NoRedirect)

    def request(self, method, path, body=None):
        request = urllib.request.Request(
            f"https://api.github.com/repos/{self.repository}/{path}",
            method=method, data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": f"Bearer {self.token}",
                     "Accept": "application/vnd.github+json", "Content-Type": "application/json",
                     "X-GitHub-Api-Version": "2022-11-28"},
        )
        try:
            with self.opener.open(request, timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise ReviewError(f"GitHub request failed (HTTP {error.code})") from None
        except (urllib.error.URLError, ValueError):
            raise ReviewError("GitHub request failed or returned invalid JSON") from None

    def get(self, path):
        return self.request("GET", path)

    def permitted(self, login):
        if not isinstance(login, str) or not re.fullmatch(r"[\w-]+", login, re.ASCII):
            return False
        result = self.get(f"collaborators/{urllib.parse.quote(login)}/permission")
        return result.get("permission") in {"admin", "maintain", "write"}

    def pull(self, number):
        return self.get(f"pulls/{number}")

    def source(self, path, sha):
        data = self.get(f"contents/{urllib.parse.quote(path, safe='/')}?ref={sha}")
        if data.get("encoding") != "base64" or data.get("size", 0) > 256 * 1024:
            return None
        return base64.b64decode(data["content"]).decode("utf-8")

    def files(self, number, expected):
        if not 0 <= expected <= MAX_FILES:
            raise ReviewError(f"Review skipped: exceeds the {MAX_FILES}-file pilot limit")
        files = self.get(f"pulls/{number}/files?per_page=100")
        if not isinstance(files, list) or len(files) != expected:
            raise ReviewError("Incomplete changed-file list; no review was run")
        return files


def requested_pr(event, event_name, repository, reviewer_login="", automatic=False):
    """Return (PR number, requesting login), or None for unrelated events."""
    if event_name == "issue_comment":
        comment = event.get("comment", {})
        if (event.get("action") != "created" or "pull_request" not in event.get("issue", {})
                or comment.get("body", "").strip() != "/review"
                or comment.get("user", {}).get("type") == "Bot"):
            return None
        return event["issue"]["number"], comment.get("user", {}).get("login", "")
    if event_name == "workflow_dispatch":
        value = str(event.get("inputs", {}).get("pr", ""))
        if not re.fullmatch(r"[1-9][0-9]*", value):
            raise ReviewError("The pr input must be a positive integer")
        return int(value), event.get("sender", {}).get("login", "")
    if event_name == "pull_request_target":
        pr = event.get("pull_request", {})
        if event.get("action") not in {"opened", "reopened", "ready_for_review", "synchronize", "review_requested"}:
            return None
        if event.get("action") == "review_requested":
            requested = event.get("requested_reviewer", {}).get("login", "")
            if not reviewer_login or requested.lower() != reviewer_login.lower():
                return None
        elif (not automatic or pr.get("draft", True)
              or (pr.get("head", {}).get("repo") or {}).get("full_name") != repository):
            return None
        if event.get("sender", {}).get("type") == "Bot":
            return None
        return pr.get("number"), event.get("sender", {}).get("login", "")
    return None


def revision(pr):
    base, head = pr["base"]["sha"], pr["head"]["sha"]
    if not all(re.fullmatch(r"[0-9a-f]{40}", value) for value in (base, head)):
        raise ReviewError("Invalid PR revision")
    return {"base": base, "head": head}


def excluded(path):
    return any(fnmatch.fnmatchcase(path, pattern) for pattern in EXCLUDES)


def right_lines(patch):
    """Map GitHub diff right-side lines, including visible context lines."""
    result, line = set(), None
    for text in patch.splitlines():
        match = re.match(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@", text)
        if match:
            line = int(match[1])
        elif line is not None and text.startswith((" ", "+")):
            result.add(line)
            line += 1
    return result


def prepare(event, event_name, github, reviewer_login="", automatic=False):
    request = requested_pr(event, event_name, github.repository, reviewer_login, automatic)
    if request is None:
        return {"status": "skipped", "reason": "This event did not request an Ollama review."}
    number, login = request
    if type(number) is not int or number <= 0:
        raise ReviewError("Invalid pull request number")
    if not github.permitted(login):
        return {"status": "skipped", "reason": "Only repository maintainers can request reviews."}
    pr = github.pull(number)
    if pr.get("state") != "open":
        return {"status": "skipped", "reason": "Pull request is no longer open."}
    rev = revision(pr)
    event_pr = event.get("pull_request")
    if event_pr and revision(event_pr) != rev:
        return {"status": "skipped", "reason": "The event refers to an older PR revision."}
    files = github.files(number, pr["changed_files"])
    if revision(github.pull(number)) != rev:
        return {"status": "skipped", "reason": "PR changed while collecting the diff."}
    eligible, omitted, patch_bytes = {}, [], 0
    for file in files:
        path = file["filename"]
        if excluded(path):
            omitted.append(path)
            continue
        patch = file.get("patch")
        if not isinstance(patch, str):
            raise ReviewError("A source patch is unavailable; no review was run")
        patch_bytes += len(patch.encode("utf-8"))
        eligible[path] = sorted(right_lines(patch))
    if patch_bytes > MAX_PATCH_BYTES:
        raise ReviewError("Review skipped: source diff exceeds the 96 KiB pilot limit")
    if not eligible:
        return {"status": "skipped", "reason": "No eligible source files changed.", "omitted": omitted}
    # Input and output share the context window; bound a call at the higher rate.
    maximum_list_cost = MODEL_CONTEXT_TOKENS * max(INPUT_USD_PER_M, OUTPUT_USD_PER_M) / 1e6
    if maximum_list_cost > BUDGET_USD:
        raise ReviewError("Configured model request exceeds the pilot cost allowance")
    return {"status": "ready", "repository": github.repository, "number": number,
            **rev, "eligible": eligible, "omitted": omitted,
            "maximum_list_cost_usd": round(maximum_list_cost, 4)}


def collect_input(request, github):
    files = github.files(request["number"], github.pull(request["number"])["changed_files"])
    selected, limited, context_bytes = [], [], 0
    for file in files:
        path = file["filename"]
        if path not in request["eligible"]:
            continue
        patch = file.get("patch")
        if not isinstance(patch, str):
            raise ReviewError("A source patch became unavailable")
        context = ""
        if len(selected) < 20 and context_bytes < MAX_CONTEXT_BYTES:
            try:
                source = github.source(path, request["base"] if file.get("status") == "removed" else request["head"])
                if source is not None:
                    lines = source.splitlines()
                    indexes = set()
                    for hunk in re.finditer(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@", patch, re.M):
                        old = file.get("status") == "removed"
                        start = int(hunk[1 if old else 3])
                        count = int(hunk[2 if old else 4] or "1")
                        indexes.update(range(max(0, start - 21), min(len(lines), start + count + 20)))
                    context = "\n".join(f"{i + 1}: {lines[i]}" for i in sorted(indexes))
            except (ReviewError, ValueError, UnicodeError):
                context = ""
        size = len(context.encode("utf-8"))
        if not context or context_bytes + size > MAX_CONTEXT_BYTES:
            limited.append(path)
            context = ""
        context_bytes += len(context.encode("utf-8"))
        selected.append({"path": path, "patch": patch, "context": context})
    if {f["path"] for f in selected} != set(request["eligible"]):
        raise ReviewError("Changed-file coverage changed while collecting the review")
    if revision(github.pull(request["number"])) != {k: request[k] for k in ("base", "head")}:
        raise ReviewError("PR changed while collecting context; request a fresh review")
    packet = {"policy": POLICY, "budget_usd": BUDGET_USD,
              "review": {"base": request["base"], "head": request["head"], "files": selected}}
    if len(json.dumps(packet).encode("utf-8")) > 256 * 1024:
        raise ReviewError("Review input exceeds the 256 KiB pilot limit")
    return packet, limited


def normalize(raw, request):
    findings_data, usage = raw.get("findings"), raw.get("usage")
    if not isinstance(findings_data, list):
        raise ReviewError("Pi returned invalid findings; no clean result is claimed")
    if not isinstance(usage, dict):
        raise ReviewError("Pi omitted usage metadata")
    if type(raw.get("requests")) is not int or raw["requests"] < 1 or raw.get("finish_reason") != "stop":
        raise ReviewError("Pi did not finish normally")
    if not all(type(usage.get(k)) is int and usage[k] >= 0 for k in ("prompt_tokens", "completion_tokens")):
        raise ReviewError("Pi did not report token usage; review remains incomplete")
    cost = (usage["prompt_tokens"] * INPUT_USD_PER_M + usage["completion_tokens"] * OUTPUT_USD_PER_M) / 1e6
    if cost > BUDGET_USD:
        raise ReviewError("Pi exceeded the review cost allowance")
    for field in ("tool_calls", "files_read", "repository_omitted"):
        if type(raw.get(field, 0)) is not int or raw.get(field, 0) < 0:
            raise ReviewError("Pi returned invalid repository usage")
    findings, fingerprints = [], set()
    for issue in findings_data:
        if not isinstance(issue, dict):
            raise ReviewError("Pi returned a malformed finding")
        path, body = issue.get("path"), issue.get("body")
        if not isinstance(path, str) or not isinstance(body, str) or not body.strip() or len(body) > 8000:
            raise ReviewError("Pi returned a malformed finding")
        fingerprint = hashlib.sha256((path + "\n" + " ".join(body.split())).encode()).hexdigest()
        if fingerprint in fingerprints:
            continue
        fingerprints.add(fingerprint)
        start, end = issue.get("start_line"), issue.get("end_line")
        visible = request["eligible"].get(path, [])
        anchored = (type(start) is int and type(end) is int and 0 < start <= end
                    and end - start < 20 and all(n in visible for n in range(start, end + 1)))
        findings.append({"path": path, "start_line": start if anchored else None,
                         "end_line": end if anchored else None, "body": body.strip(),
                         "fingerprint": fingerprint})
    return {"status": "complete", "model": MODEL,
            "thinking": "low", "base": request["base"], "head": request["head"],
            "findings": findings, "omitted": request["omitted"], "requests": raw["requests"],
            **{field: raw.get(field, 0) for field in ("tool_calls", "files_read", "repository_omitted")},
            "usage": {k: usage[k] for k in ("prompt_tokens", "completion_tokens")},
            "estimated_cost_usd": round(cost, 6)}


def snapshot(request, directory, github):
    """Fetch inert Git objects; never check out or execute PR files."""
    root = directory / "repository.git"
    env = {"PATH": os.environ["PATH"], "HOME": str(directory), "GIT_CONFIG_NOSYSTEM": "1",
           "GIT_CONFIG_GLOBAL": os.devnull, "GIT_TERMINAL_PROMPT": "0"}
    credential = base64.b64encode(f"x-access-token:{github.token}".encode()).decode()
    authenticated = {**env, "GIT_CONFIG_COUNT": "1",
                     "GIT_CONFIG_KEY_0": "http.https://github.com/.extraheader",
                     "GIT_CONFIG_VALUE_0": f"AUTHORIZATION: basic {credential}"}

    def git(args, authenticate=False):
        try:
            process = subprocess.run(["git", "--no-replace-objects", *args],
                                     env=authenticated if authenticate else env,
                                     stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=60)
        except subprocess.TimeoutExpired:
            raise ReviewError("Repository snapshot timed out") from None
        if process.returncode:
            raise ReviewError("Could not prepare the read-only repository snapshot")
        return process.stdout

    git(["init", "--bare", "--quiet", str(root)])
    prefix = ["--git-dir", str(root), "-c", "core.hooksPath=" + os.devnull]
    git([*prefix, "fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules",
         f"https://github.com/{github.repository}.git", f"refs/pull/{request['number']}/head"], True)
    if git([*prefix, "rev-parse", "FETCH_HEAD"]).decode().strip() != request["head"]:
        raise ReviewError("PR changed while fetching the repository snapshot")
    tree = git([*prefix, "ls-tree", "-r", "-l", "-z", request["head"]])
    if len(tree) > 8 * 1024 * 1024:
        raise ReviewError("Repository file index exceeds the pilot size limit")
    files = []
    omitted = 0
    for record in tree.split(b"\0"):
        if not record:
            continue
        metadata, path = record.split(b"\t", 1)
        mode, kind, oid, size = metadata.decode("ascii").split()
        try:
            path = path.decode("utf-8")
        except UnicodeError:
            omitted += 1
            continue
        if (kind != "blob" or mode not in {"100644", "100755"}
                or excluded(path) or int(size) > 256 * 1024):
            omitted += 1
            continue
        files.append({"path": path, "oid": oid, "size": int(size)})
    return {"git_dir": str(root), "head": request["head"], "files": files, "omitted": omitted}


def analyze(request, directory, github, key):
    if not key:
        raise ReviewError("Add the repository Actions secret OLLAMA_API_KEY before testing")
    current = github.pull(request["number"])
    if current.get("state") != "open" or revision(current) != {k: request[k] for k in ("base", "head")}:
        raise ReviewError("PR changed before analysis; request a fresh review")
    packet, limited = collect_input(request, github)
    repository = snapshot(request, directory, github)
    index = directory / "repository.json"
    index.write_text(json.dumps(repository))
    source, output = directory / "input.json", directory / "engine.json"
    source.write_text(json.dumps(packet))
    source.chmod(0o600)
    output.unlink(missing_ok=True)
    command = ["node", str(Path(__file__).with_name("engine.mjs")), str(source), str(output), str(index)]
    env = {"PATH": os.environ["PATH"], "HOME": str(directory), "OLLAMA_API_KEY": key}
    started = time.monotonic()
    try:
        process = subprocess.run(command, cwd=directory, env=env,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=540)
    except subprocess.TimeoutExpired:
        raise ReviewError("Pi review exceeded its runtime limit") from None
    if not output.is_file() or output.stat().st_size > 1024 * 1024:
        raise ReviewError(f"Pi produced no result (exit {process.returncode}); check the pinned Node runtime and npm install")
    raw = json.loads(output.read_text())
    if process.returncode or raw.get("error"):
        error = raw.get("error")
        metrics = {}
        usage = raw.get("usage")
        if (isinstance(usage, dict) and all(type(usage.get(k)) is int and usage[k] >= 0
                for k in ("prompt_tokens", "completion_tokens"))):
            metrics["usage"] = {k: usage[k] for k in ("prompt_tokens", "completion_tokens")}
            metrics["estimated_cost_usd"] = round((usage["prompt_tokens"] * INPUT_USD_PER_M
                + usage["completion_tokens"] * OUTPUT_USD_PER_M) / 1e6, 6)
        for field in ("requests", "tool_calls", "files_read"):
            if type(raw.get(field)) is int and raw[field] >= 0:
                metrics[field] = raw[field]
        raise ReviewError(error if isinstance(error, str) and len(error) <= 500 else "Pi review failed", metrics)
    pr = github.pull(request["number"])
    if pr.get("state") != "open" or revision(pr) != {k: request[k] for k in ("base", "head")}:
        raise ReviewError("PR changed during analysis; result discarded")
    reviewed = normalize(raw, request)
    reviewed["context_limited"] = limited
    reviewed["requests"] = raw["requests"]
    reviewed["repository_omitted"] = repository["omitted"]
    reviewed["seconds"] = round(time.monotonic() - started, 1)
    return reviewed


def summary(result):
    escaped = lambda value: html.escape(str(value))
    status = result.get("status", "incomplete")
    lines = [f"## Ollama review: {status}", ""]
    if result.get("reason"):
        lines.append(escaped(result["reason"]))
    if result.get("head"):
        lines += [f"Reviewed head: `{escaped(result['head'])}`",
                  f"Model: `{MODEL}`, thinking: `low`. PR feedback is published by the separate feedback job.", ""]
    for finding in result.get("findings", []):
        location = finding["path"]
        if finding["start_line"]:
            location += f":{finding['start_line']}"
        else:
            location += " (no validated inline location)"
        lines += [f"<h3>{escaped(location)}</h3>", f"<pre>{escaped(finding['body'])}</pre>"]
    if status == "complete" and not result.get("findings"):
        lines.append("No actionable findings in the analyzed changes. This does not establish correctness.")
    for field in ("omitted", "context_limited"):
        if result.get(field):
            lines += [f"<p>{field}:</p><pre>{escaped(chr(10).join(result[field]))}</pre>"]
    if "usage" in result:
        lines.append(f"Model requests: {result.get('requests', 1)}. Repository tool calls: {result.get('tool_calls', 0)}; "
                     f"distinct files read with read_file: {result.get('files_read', 0)}. "
                     f"Repository files unavailable to tools: {result.get('repository_omitted', 0)}.")
        lines.append(f"<pre>{escaped(json.dumps(result['usage']))}</pre>")
        lines.append(f"Estimated list-price cost: ${result['estimated_cost_usd']:.4f}; actual billing may differ.")
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["prepare", "analyze", "summary", "export"])
    parser.add_argument("--directory", type=Path, required=True)
    args = parser.parse_args()
    args.directory.mkdir(parents=True, exist_ok=True)
    path = args.directory / "result.json"
    try:
        if args.command == "summary":
            result = json.loads(path.read_text()) if path.exists() else {"status": "incomplete", "reason": "Review did not complete."}
            with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as target:
                target.write(summary(result))
            return
        if args.command == "export":
            result = json.loads(path.read_text()) if path.exists() else {}
            with open(os.environ["GITHUB_OUTPUT"], "a") as target:
                target.write(f"review_result={json.dumps(result)}\n")
            return
        github = GitHub(os.environ["GITHUB_REPOSITORY"], os.environ.get("GITHUB_TOKEN", ""))
        if args.command == "prepare":
            event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
            result = prepare(event, os.environ["GITHUB_EVENT_NAME"], github,
                             os.environ.get("REVIEWER_LOGIN", ""), os.environ.get("AUTO_REVIEW") == "true")
            (args.directory / "request.json").write_text(json.dumps(result))
            with open(os.environ["GITHUB_OUTPUT"], "a") as target:
                target.write(f"ready={'true' if result['status'] == 'ready' else 'false'}\n")
                if result["status"] == "ready":
                    target.write(f"number={result['number']}\n")
                    target.write(f"request={json.dumps(result)}\n")
        else:
            request = json.loads(os.environ["REVIEW_REQUEST"])
            result = analyze(request, args.directory, github, os.environ.get("OLLAMA_API_KEY", ""))
        path.write_text(json.dumps(result, indent=2))
        if args.command == "analyze":
            usage = result["usage"]
            print(f"Pi review complete: {result['requests']} request; "
                  f"{usage['prompt_tokens']} input tokens; {usage['completion_tokens']} output tokens; "
                  f"estimated list-price cost ${result['estimated_cost_usd']:.4f}; "
                  f"{result['seconds']} seconds")
    except (ReviewError, ValueError, KeyError, OSError) as error:
        reason = str(error) if isinstance(error, ReviewError) else "Review failed while validating local or remote data."
        metrics = error.metrics if isinstance(error, ReviewError) else {}
        path.write_text(json.dumps({"status": "incomplete", "reason": reason, **metrics}))
        print(reason, file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    def interrupted(signum, frame):
        raise ReviewError("Review was cancelled")

    signal.signal(signal.SIGTERM, interrupted)
    main()
