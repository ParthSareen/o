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

    def files(self, number, expected):
        return self.changed


def response(issues=None):
    return {"review": {"key_issues_to_review": issues or []},
            "coverage": {"files_not_reviewed": [], "files_partially_reviewed": []},
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
        issue = {"relevant_file": "app.py", "issue_content": "A real bug", "start_line": 3, "end_line": 3}
        unanchored = {"relevant_file": "elsewhere.py", "issue_content": "Needs context", "start_line": 900, "end_line": 900}
        result = review.normalize(response([issue, copy.deepcopy(issue), unanchored]), REQUEST)
        self.assertEqual(len(result["findings"]), 2)
        self.assertEqual(result["findings"][0]["start_line"], 3)
        self.assertIsNone(result["findings"][1]["start_line"])
        raw = response()
        raw["coverage"]["files_partially_reviewed"] = ["app.py"]
        self.assertEqual(review.normalize(raw, REQUEST)["status"], "partial")

    def test_invalid_truncated_or_unmetered_results_never_report_clean(self):
        cases = [response(), response(), response(), response()]
        cases[0]["review"]["key_issues_to_review"] = "none"
        cases[1]["usage"] = {}
        cases[2]["usage"]["completion_tokens"] = review.MAX_OUTPUT_TOKENS
        cases[3].pop("coverage")
        for raw in cases:
            with self.subTest(raw=raw), self.assertRaises(review.ReviewError):
                review.normalize(raw, REQUEST)

    def test_changed_head_discards_output_and_engine_gets_no_write_token(self):
        github = FakeGitHub()
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)

            def execute(command, **kwargs):
                if command[1] == "run":
                    (directory / "engine.json").write_text(json.dumps(response()))
                    github.pr["head"]["sha"] = "d" * 40
                return subprocess.CompletedProcess(command, 0)

            with patch.object(review.subprocess, "run", side_effect=execute) as run:
                with self.assertRaisesRegex(review.ReviewError, "changed during"):
                    review.analyze(REQUEST, directory, github, "not-a-real-key")
                self.assertEqual(run.call_count, 2)  # one engine invocation and cleanup
            env = review.engine_environment("read-token", "key")
            self.assertEqual(env["CONFIG__PUBLISH_OUTPUT"], "false")
            self.assertEqual(env["CONFIG__NUM_RETRIES"], "0")
            self.assertEqual(env["CONFIG__FALLBACK_MODELS"], "[]")

    def test_successful_analysis_reports_engine_filtered_files(self):
        github = FakeGitHub()
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)

            def execute(command, **kwargs):
                if command[1] == "run":
                    (directory / "engine.json").write_text(json.dumps(response()))
                    (directory / "audit.json").write_text(json.dumps(
                        {"requests": 1, "finish_reason": "stop", "files": []}))
                return subprocess.CompletedProcess(command, 0)

            with patch.object(review.subprocess, "run", side_effect=execute):
                result = review.analyze(REQUEST, directory, github, "not-a-real-key")
            self.assertEqual(result["omitted"], ["app.py"])
            self.assertEqual(result["status"], "partial")
            self.assertEqual(result["requests"], 1)

    def test_summary_escapes_model_output(self):
        issue = {"relevant_file": "app.py", "issue_content": "</pre><script>alert(1)</script>", "start_line": 3, "end_line": 3}
        text = review.summary(review.normalize(response([issue]), REQUEST))
        self.assertNotIn("<script>", text)
        self.assertIn("&lt;script&gt;", text)
        self.assertNotIn("No actionable findings", text)


if __name__ == "__main__":
    unittest.main()
