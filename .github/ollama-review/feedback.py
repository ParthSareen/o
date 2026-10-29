"""Publish PR status and validated findings without a model credential."""

import argparse
import html
import json
import os
import re

from review import GitHub, MODEL, ReviewError, normalize, revision, right_lines


STATUS = re.compile(r"^<!-- ollama-review-status run=(\d+) attempt=(\d+) -->")
BOT = "github-actions[bot]"


def owned(comment):
    user = comment.get("user", {})
    return user.get("login") == BOT and user.get("type") == "Bot"


def pages(github, path):
    for page in range(1, 101):
        values = github.get(f"{path}?per_page=100&page={page}")
        if not isinstance(values, list):
            raise ReviewError("Invalid GitHub comment list")
        yield from values
        if len(values) < 100:
            return
    raise ReviewError("Comment history exceeds the pagination limit")


def current_comment(github, number):
    comments = [c for c in pages(github, f"issues/{number}/comments")
                if owned(c) and STATUS.match(c.get("body", ""))]
    return max(comments, key=lambda c: c["id"]) if comments else None


def safe_text(value):
    return html.escape(str(value)).replace("@", "@\u200b").replace("![", "\\![")


def body(request, state, run_id, attempt, result=None, note=""):
    run_url = f"https://github.com/{request['repository']}/actions/runs/{run_id}"
    titles = {"queued": "Queued", "running": "Running", "complete": "Complete",
              "cancelled": "Cancelled", "stale": "Superseded", "incomplete": "Could not complete"}
    lines = [f"<!-- ollama-review-status run={run_id} attempt={attempt} -->",
             f"### Ollama review — {titles[state]}", "",
             f"[View run]({run_url}) · Head `{request['head'][:12]}` · `{MODEL}` via Pi", ""]
    if state == "queued":
        lines.append("Acknowledged. Your review request was accepted and queued.")
    elif state == "running":
        lines.append("The review pipeline has started. Preparing the runtime and analyzing the change.")
    elif state == "cancelled":
        lines.append("This run was cancelled. A newer request may be running.")
    elif state == "stale":
        lines.append("The PR changed before publication. Request a fresh review with `/review`.")
    elif state == "incomplete":
        lines.append(safe_text(note or "The pipeline failed before a complete review was available. See the run for details."))
    elif result:
        findings = result["findings"]
        if not findings:
            lines.append("No actionable findings in the analyzed changes. This does not establish correctness.")
        for index, finding in enumerate(findings, 1):
            location = finding["path"]
            location += f":{finding['start_line']}" if finding["start_line"] else " (no validated inline location)"
            lines += [f"#### {index}. <code>{safe_text(location)}</code>", "", safe_text(finding["body"]), ""]
        for name in ("omitted", "context_limited"):
            if result.get(name):
                lines += [f"{name.replace('_', ' ').capitalize()}: " + ", ".join(
                    f"<code>{safe_text(path)}</code>" for path in result[name]), ""]
        usage = result["usage"]
        lines += [f"{usage['prompt_tokens']:,} input tokens · {usage['completion_tokens']:,} output tokens · "
                  f"estimated list-price cost ${result['estimated_cost_usd']:.4f}",
                  "Automated findings need verification. This review does not approve the PR."]
    if note and state != "incomplete":
        lines += ["", safe_text(note)]
    rendered = "\n".join(lines)
    if len(rendered.encode("utf-8")) > 60000:
        rendered = rendered.encode("utf-8")[:59000].decode("utf-8", errors="ignore")
        rendered += "\n\nComment truncated; see the linked Actions summary for full output."
    return rendered


def update(github, request, state, run_id, attempt, result=None, note=""):
    existing = current_comment(github, request["number"])
    if existing:
        marker = STATUS.match(existing["body"])
        previous = (int(marker[1]), int(marker[2]))
        current = (run_id, attempt)
        if previous > current or (state != "queued" and previous != current):
            return False
    elif state != "queued":
        return False
    payload = {"body": body(request, state, run_id, attempt, result, note)}
    if existing:
        github.request("PATCH", f"issues/comments/{existing['id']}", payload)
    else:
        github.request("POST", f"issues/{request['number']}/comments", payload)
    return True


def validate_result(github, request, result):
    if result.get("status") != "complete":
        raise ReviewError(result.get("reason", "Review did not complete."))
    expected = {k: request[k] for k in ("base", "head")}
    if any(result.get(k) != v for k, v in expected.items()):
        raise ReviewError("Review result belongs to a different revision")
    pr = github.pull(request["number"])
    if pr.get("state") != "open" or revision(pr) != expected:
        return None
    files = github.files(request["number"], pr["changed_files"])
    verified = dict(request)
    verified["eligible"] = {file["filename"]: sorted(right_lines(file.get("patch", ""))) for file in files
                            if file["filename"] in request["eligible"]}
    validated = normalize({"findings": result.get("findings"), "usage": result.get("usage"),
                           "requests": result.get("requests"), "finish_reason": "stop"}, verified)
    context = result.get("context_limited", [])
    if isinstance(context, list) and all(isinstance(value, str) for value in context):
        validated["context_limited"] = context[:100]
    return validated


def post_inline(github, request, result, run_id, attempt):
    number = request["number"]
    marker = f"<!-- ollama-review-run {run_id}/{attempt} -->"
    if any(owned(r) and marker in r.get("body", "") for r in pages(github, f"pulls/{number}/reviews")):
        return
    previous = list(pages(github, f"pulls/{number}/comments"))
    comments = []
    for finding in result["findings"]:
        if finding["start_line"] is None:
            continue
        fingerprint = f"<!-- ollama-review-finding {finding['fingerprint']} -->"
        if any(owned(c) and (fingerprint in c.get("body", "") or (
                c.get("commit_id") == request["head"] and c.get("path") == finding["path"]
                and c.get("line") == finding["end_line"]
                and "<!-- ollama-review-finding " in c.get("body", ""))) for c in previous):
            continue
        comment = {"path": finding["path"], "side": "RIGHT", "line": finding["end_line"],
                   "body": fingerprint + "\n" + safe_text(finding["body"])}
        if finding["start_line"] != finding["end_line"]:
            comment.update(start_line=finding["start_line"], start_side="RIGHT")
        comments.append(comment)
    if comments:
        github.request("POST", f"pulls/{number}/reviews", {
            "commit_id": request["head"], "event": "COMMENT", "comments": comments,
            "body": marker + f"\nAutomated review of `{request['head'][:12]}`. Please verify these findings.",
        })


def finish(github, request, result, job_result, run_id, attempt):
    existing = current_comment(github, request["number"])
    if not existing or not STATUS.match(existing["body"]):
        return False
    marker = STATUS.match(existing["body"])
    if (int(marker[1]), int(marker[2])) != (run_id, attempt):
        return False
    if job_result == "cancelled":
        return update(github, request, "cancelled", run_id, attempt)
    if job_result != "success":
        return update(github, request, "incomplete", run_id, attempt,
                      note=result.get("reason", "Pipeline failed before review completion. See the run for details."))
    try:
        validated = validate_result(github, request, result)
    except ReviewError as error:
        return update(github, request, "incomplete", run_id, attempt, note=str(error))
    if validated is None:
        return update(github, request, "stale", run_id, attempt)
    note = ""
    try:
        if revision(github.pull(request["number"])) != {k: request[k] for k in ("base", "head")}:
            return update(github, request, "stale", run_id, attempt)
        post_inline(github, request, validated, run_id, attempt)
    except ReviewError:
        note = "Inline feedback could not be confirmed; all findings are included here. No automatic retry was made."
    if revision(github.pull(request["number"])) != {k: request[k] for k in ("base", "head")}:
        return update(github, request, "stale", run_id, attempt)
    return update(github, request, "complete", run_id, attempt, validated, note)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=["queued", "running", "finished"])
    args = parser.parse_args()
    github = GitHub(os.environ["GITHUB_REPOSITORY"], os.environ["GITHUB_TOKEN"])
    request = json.loads(os.environ["REVIEW_REQUEST"])
    run_id, attempt = int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"])
    if request["repository"] != github.repository:
        raise ReviewError("Review belongs to a different repository")
    if args.phase == "finished":
        result = json.loads(os.environ.get("REVIEW_RESULT") or "{}")
        active = finish(github, request, result, os.environ["REVIEW_JOB_RESULT"], run_id, attempt)
    else:
        active = update(github, request, args.phase, run_id, attempt)
    if args.phase == "queued":
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"active={'true' if active else 'false'}\n")


if __name__ == "__main__":
    main()
