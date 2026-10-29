import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import review


BASE = "a" * 40
HEAD = "b" * 40
SNAPSHOT = {"git_dir": "/unused-test-snapshot", "head": HEAD, "files": [], "omitted": 0}
PR = {"number": 7, "state": "open", "draft": False, "changed_files": 1,
      "base": {"sha": BASE}, "head": {"sha": HEAD, "repo": {"full_name": "owner/repo"}}}
PATCH = "@@ -2,3 +2,3 @@\n old\n-removed\n+added\n tail\n"
REQUEST = {"status": "ready", "number": 7, "repository": "owner/repo",
           "base": BASE, "head": HEAD, "eligible": {"app.py": [2, 3, 4]}, "omitted": []}


def comment(body="/review", login="owner"):
    return {"action": "created", "issue": {"number": 7, "pull_request": {}},
            "comment": {"body": body, "user": {"type": "User", "login": login}}}


class FakeGitHub:
    repository = "owner/repo"
    token = "not-a-real-token"

    def __init__(self, allowed=True):
        self.allowed = allowed
        self.pr = copy.deepcopy(PR)
        self.changed = [{"filename": "app.py", "patch": PATCH}]
        self.reads = 0

    def permitted(self, login):
        return self.allowed

    def pull(self, number):
        self.reads += 1
        return copy.deepcopy(self.pr)

    def source(self, path, sha):
        return "line one\nline two\nline three\nline four\n"

    def files(self, number, expected):
        return self.changed


def response(issues=None):
    return {"findings": issues or [], "requests": 1, "finish_reason": "stop",
            "usage": {"prompt_tokens": 1000, "completion_tokens": 100}}


class ReviewTest(unittest.TestCase):
    def test_commands_are_exact_and_reviewer_hook_is_opt_in(self):
        self.assertEqual(review.requested_pr(comment(), "issue_comment", "owner/repo"), (7, "owner"))
        for text in ("please /review", "/review --publish", "/review\nrun this"):
            self.assertIsNone(review.requested_pr(comment(text), "issue_comment", "owner/repo"))
        bot = comment()
        bot["comment"]["user"]["type"] = "Bot"
        self.assertIsNone(review.requested_pr(bot, "issue_comment", "owner/repo"))
        event = {"action": "review_requested", "pull_request": PR,
                 "requested_reviewer": {"login": "review-account"}, "sender": {"login": "owner"}}
        self.assertIsNone(review.requested_pr(event, "pull_request_target", "owner/repo"))
        self.assertEqual(review.requested_pr(event, "pull_request_target", "owner/repo", "review-account"), (7, "owner"))

    def test_automatic_forks_and_unrelated_events_do_not_run(self):
        event = {"action": "synchronize", "pull_request": copy.deepcopy(PR), "sender": {"login": "owner"}}
        self.assertIsNone(review.requested_pr(event, "pull_request_target", "owner/repo"))
        self.assertEqual(review.requested_pr(event, "pull_request_target", "owner/repo", automatic=True), (7, "owner"))
        event["pull_request"]["head"]["repo"]["full_name"] = "fork/repo"
        self.assertIsNone(review.requested_pr(event, "pull_request_target", "owner/repo", automatic=True))
        event["pull_request"]["head"]["repo"] = None
        self.assertIsNone(review.requested_pr(event, "pull_request_target", "owner/repo", automatic=True))

    def test_untrusted_comment_cannot_spend_or_read_the_pr(self):
        github = FakeGitHub(allowed=False)
        self.assertEqual(review.prepare(comment(login="outsider"), "issue_comment", github)["status"], "skipped")
        self.assertEqual(github.reads, 0)
        with self.assertRaises(review.ReviewError):
            review.requested_pr({"inputs": {"pr": "7; echo token"}}, "workflow_dispatch", "owner/repo")

    def test_diff_snapshot_exclusions_and_missing_source(self):
        github = FakeGitHub()
        github.changed.append({"filename": "private.key"})
        result = review.prepare(comment(), "issue_comment", github)
        self.assertEqual(result["eligible"], {"app.py": [2, 3, 4]})
        self.assertEqual(result["omitted"], ["private.key"])
        self.assertEqual((result["base"], result["head"]), (BASE, HEAD))
        github.changed[0].pop("patch")
        with self.assertRaisesRegex(review.ReviewError, "source patch"):
            review.prepare(comment(), "issue_comment", github)
        github.changed = [{"filename": "app.py", "patch": "x" * (review.MAX_PATCH_BYTES + 1)}]
        with self.assertRaisesRegex(review.ReviewError, "96 KiB"):
            review.prepare(comment(), "issue_comment", github)

    def test_stale_events_do_not_review_the_new_head(self):
        github = FakeGitHub()
        event = {"action": "synchronize", "pull_request": copy.deepcopy(PR), "sender": {"login": "owner"}}
        event["pull_request"]["head"]["sha"] = "c" * 40
        self.assertEqual(review.prepare(event, "pull_request_target", github, automatic=True)["status"], "skipped")

    def test_findings_deduplicate_and_invalid_lines_fall_back_to_summary(self):
        issue = {"path": "app.py", "body": "A real bug", "start_line": 3, "end_line": 3}
        unanchored = {"path": "elsewhere.py", "body": "Needs context", "start_line": 900, "end_line": 900}
        result = review.normalize(response([issue, copy.deepcopy(issue), unanchored]), REQUEST)
        self.assertEqual(len(result["findings"]), 2)
        self.assertEqual(result["findings"][0]["start_line"], 3)
        self.assertIsNone(result["findings"][1]["start_line"])

    def test_invalid_truncated_or_unmetered_results_never_report_clean(self):
        cases = [response() for _ in range(6)]
        cases[0]["findings"] = "none"
        cases[1]["usage"] = {}
        cases[2]["usage"]["completion_tokens"] = -1
        cases[3]["finish_reason"] = "length"
        cases[4]["requests"] = 0
        cases[5]["usage"]["completion_tokens"] = 5_000_000
        for raw in cases:
            with self.subTest(raw=raw), self.assertRaises(review.ReviewError):
                review.normalize(raw, REQUEST)

    def test_all_distinct_findings_survive_normalization_and_summary(self):
        issues = [{"path": "app.py", "start_line": 3, "end_line": 3,
                   "body": f"Distinct bug {i}."} for i in range(12)]
        result = review.normalize(response(issues + [copy.deepcopy(issues[0])]), REQUEST)
        self.assertEqual(len(result["findings"]), len(issues))
        rendered = review.summary(result)
        for issue in issues:
            self.assertIn(issue["body"], rendered)

    def test_multiple_model_turns_preserve_cumulative_usage(self):
        raw = response()
        raw["requests"] = 4
        result = review.normalize(raw, REQUEST)
        self.assertEqual(result["requests"], 4)
        self.assertEqual(result["usage"], raw["usage"])

    def test_snapshot_pins_head_and_filters_non_source_without_persisting_credentials(self):
        github = FakeGitHub()
        tree = (f"100644 blob {'c' * 40} 10\tsrc/caller.py\0"
                f"100644 blob {'d' * 40} 20\tsrc/unchanged.py\0"
                f"120000 blob {'e' * 40} 11\tlink.py\0"
                f"100644 blob {'f' * 40} 9\t.env\0"
                f"160000 commit {'a' * 40} -\tsubmodule\0").encode()
        def execute(command, **kwargs):
            if "fetch" in command:
                self.assertIn("refs/pull/7/head", command)
                self.assertIn("GIT_CONFIG_VALUE_0", kwargs["env"])
                self.assertNotIn(github.token, " ".join(command))
            else:
                self.assertNotIn("GIT_CONFIG_VALUE_0", kwargs["env"])
            output = HEAD.encode() if "rev-parse" in command else tree if "ls-tree" in command else b""
            return subprocess.CompletedProcess(command, 0, stdout=output)
        with tempfile.TemporaryDirectory() as tmp, patch.object(review.subprocess, "run", side_effect=execute):
            result = review.snapshot(REQUEST, Path(tmp), github)
        self.assertEqual([item["path"] for item in result["files"]], ["src/caller.py", "src/unchanged.py"])
        self.assertEqual(result["omitted"], 3)

        with tempfile.TemporaryDirectory() as tmp, patch.object(review.subprocess, "run",
                return_value=subprocess.CompletedProcess([], 0, stdout=b"wrong-head")):
            with self.assertRaisesRegex(review.ReviewError, "changed while fetching"):
                review.snapshot(REQUEST, Path(tmp), github)

    def test_changed_head_discards_output_and_engine_gets_no_write_token(self):
        github = FakeGitHub()
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)

            def execute(command, **kwargs):
                if command[0] == "node":
                    (directory / "engine.json").write_text(json.dumps(response()))
                    github.pr["head"]["sha"] = "d" * 40
                return subprocess.CompletedProcess(command, 0)

            with patch.object(review, "snapshot", return_value=SNAPSHOT), patch.object(review.subprocess, "run", side_effect=execute) as run:
                with self.assertRaisesRegex(review.ReviewError, "changed during"):
                    review.analyze(REQUEST, directory, github, "not-a-real-key")
                self.assertEqual(run.call_count, 1)
                self.assertNotIn("GITHUB_TOKEN", run.call_args.kwargs["env"])
    def test_successful_analysis_reports_missing_context(self):
        github = FakeGitHub()
        github.source = lambda path, sha: None
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)

            def execute(command, **kwargs):
                (directory / "engine.json").write_text(json.dumps(response()))
                packet = json.loads((directory / "input.json").read_text())
                self.assertEqual(packet["review"]["files"][0]["patch"], PATCH)
                self.assertEqual(packet["review"]["files"][0]["context"], "")
                self.assertEqual(packet["budget_usd"], 5)
                return subprocess.CompletedProcess(command, 0)

            with patch.object(review, "snapshot", return_value=SNAPSHOT), patch.object(review.subprocess, "run", side_effect=execute):
                result = review.analyze(REQUEST, directory, github, "not-a-real-key")
            self.assertEqual(result["context_limited"], ["app.py"])
            self.assertEqual(result["requests"], 1)

    def test_failed_analysis_preserves_metering_without_provider_details(self):
        github = FakeGitHub()
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            def execute(command, **kwargs):
                (directory / "engine.json").write_text(json.dumps({
                    "error": "Budget exhausted.", "requests": 3, "tool_calls": 2, "files_read": 1,
                    "usage": {"prompt_tokens": 100000, "completion_tokens": 400000},
                    "raw_provider_detail": "not safe to publish",
                }))
                return subprocess.CompletedProcess(command, 1)
            with patch.object(review, "snapshot", return_value=SNAPSHOT), patch.object(review.subprocess, "run", side_effect=execute):
                with self.assertRaises(review.ReviewError) as raised:
                    review.analyze(REQUEST, directory, github, "not-a-real-key")
            metrics = raised.exception.metrics
            self.assertEqual(metrics["requests"], 3)
            self.assertEqual(metrics["estimated_cost_usd"], 0.51)
            text = review.summary({"status": "incomplete", "reason": str(raised.exception), **metrics})
            self.assertIn("400000", text)
            self.assertNotIn("not safe to publish", text)

    def test_summary_escapes_model_output(self):
        issue = {"path": "app.py", "body": "</pre><script>alert(1)</script>", "start_line": 3, "end_line": 3}
        text = review.summary(review.normalize(response([issue]), REQUEST))
        self.assertNotIn("<script>", text)
        self.assertIn("&lt;script&gt;", text)
        self.assertNotIn("No actionable findings", text)


if __name__ == "__main__":
    unittest.main()
