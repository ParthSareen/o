import base64
import copy
import json
from pathlib import Path
import subprocess
import os
import sys
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
    return {"findings": issues or [], "requests": 1, "finish_reason": "submitted", "submission_calls": 1,
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

    def test_command_whitespace_is_trimmed_but_extra_text_cannot_trigger_inference(self):
        for mode, command in (("repository", "/review"), ("diff", "/review-diff")):
            for body in (command, command + "\r\n", command + "\n", "  " + command + " \n"):
                with self.subTest(mode=mode, body=body):
                    self.assertEqual(review.requested_pr(comment(body), "issue_comment", "owner/repo", mode=mode), (7, "owner"))
            for body in ("please " + command, command + " extra", command + "\nrun this"):
                with self.subTest(mode=mode, body=body):
                    github = FakeGitHub()
                    self.assertEqual(review.prepare(comment(body), "issue_comment", github, mode=mode)["status"], "skipped")
                    self.assertEqual(github.reads, 0)

    def test_review_diff_routes_to_its_own_manual_reviewer(self):
        self.assertEqual(review.requested_pr(comment("/review-diff"), "issue_comment", "owner/repo", mode="diff"), (7, "owner"))
        self.assertIsNone(review.requested_pr(comment("/review-diff"), "issue_comment", "owner/repo"))
        self.assertIsNone(review.requested_pr(comment(), "issue_comment", "owner/repo", mode="diff"))
        self.assertIsNone(review.requested_pr(comment("/review-diff extra"), "issue_comment", "owner/repo", mode="diff"))
        self.assertEqual(review.prepare(comment("/review-diff", login="outsider"), "issue_comment", FakeGitHub(False), mode="diff")["status"], "skipped")
        event = {"action": "synchronize", "pull_request": PR, "sender": {"login": "owner"}}
        self.assertIsNone(review.requested_pr(event, "pull_request_target", "owner/repo", automatic=True, mode="diff"))

    def test_diff_analysis_passes_context_without_preparing_a_repository(self):
        github = FakeGitHub()
        request = review.prepare(comment("/review-diff"), "issue_comment", github, mode="diff")
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            def execute(command, **kwargs):
                packet = json.loads((directory / "input.json").read_text())
                self.assertEqual(packet["mode"], "diff")
                self.assertEqual(packet["review"]["files"][0]["patch"], PATCH)
                self.assertIn("3: line three", packet["review"]["files"][0]["context"])
                self.assertIn("no repository search/read tools", packet["policy"])
                self.assertFalse((directory / "repository.json").exists())
                (directory / "engine.json").write_text(json.dumps(response()))
                return subprocess.CompletedProcess(command, 0)
            with patch.object(review, "snapshot") as snapshot, patch.object(review.subprocess, "run", side_effect=execute):
                output = review.analyze(request, directory, github, "fake-key")
                snapshot.assert_not_called()
            self.assertEqual(output["mode"], "diff")
        for raw in ({**response(), "requests": 2}, {**response(), "tool_calls": 1}):
            with self.assertRaisesRegex(review.ReviewError, "one model request"):
                review.normalize(raw, request)

    def test_diff_entry_point_writes_its_own_summary_without_credentials(self):
        entry = Path(__file__).resolve().parent.parent / "ollama-review-diff" / "review.py"
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            (directory / "result.json").write_text(json.dumps({"status": "complete", "mode": "diff", "findings": []}))
            summary = directory / "summary.md"
            subprocess.run([sys.executable, str(entry), "summary", "--directory", str(directory)], check=True,
                           env={"PATH": os.environ["PATH"], "GITHUB_STEP_SUMMARY": str(summary)})
            self.assertIn("Ollama diff review: complete", summary.read_text())

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
        unanchored = {"path": "app.py", "body": "Needs context", "start_line": 900, "end_line": 900}
        result = review.normalize(response([issue, copy.deepcopy(issue), unanchored]), REQUEST)
        self.assertEqual(len(result["findings"]), 2)
        self.assertEqual(result["findings"][0]["start_line"], 3)
        self.assertIsNone(result["findings"][1]["start_line"])

    def test_findings_must_belong_to_the_reviewed_change(self):
        with self.assertRaisesRegex(review.ReviewError, "outside the reviewed"):
            review.normalize(response([{"path": "unrelated.py", "body": "Invented location"}]), REQUEST)

    def test_source_context_never_follows_symlinks_or_reads_excluded_files(self):
        github = review.GitHub("owner/repo", "fake-token")
        secret = {"encoding": "base64", "size": 6, "content": base64.b64encode(b"secret").decode()}
        tree = {"tree": [{"path": "helper.py", "type": "blob", "mode": "120000", "sha": "c" * 40},
                         {"path": ".env", "type": "blob", "mode": "100644", "sha": "d" * 40}]}
        with patch.object(github, "get", side_effect=lambda path: tree if path.startswith("git/trees/") else secret) as get:
            self.assertIsNone(github.source("helper.py", HEAD))
            self.assertIsNone(github.source(".env", HEAD))
            self.assertEqual(get.call_count, 1)

    def test_source_context_reads_only_regular_blobs_and_rejects_truncated_trees(self):
        github = review.GitHub("owner/repo", "fake-token")
        tree = {"tree": [{"path": "app.py", "type": "blob", "mode": "100644", "sha": "c" * 40}]}
        content = {"encoding": "base64", "size": 4, "content": base64.b64encode(b"code").decode()}
        with patch.object(github, "get", side_effect=[tree, content, content]) as get:
            self.assertEqual(github.source("app.py", HEAD), "code")
            self.assertEqual(github.source("app.py", HEAD), "code")
            self.assertEqual(get.call_args.args[0], f"git/blobs/{'c' * 40}")
        github = review.GitHub("owner/repo", "fake-token")
        with patch.object(github, "get", return_value={**tree, "truncated": True}) as get:
            self.assertIsNone(github.source("app.py", HEAD))
            self.assertEqual(get.call_count, 1)

    def test_explicit_profile_and_budget_are_preserved_in_review_input(self):
        github = FakeGitHub()
        request = review.prepare(comment(), "issue_comment", github, profile="security", budget_usd=0.5)
        packet, _ = review.collect_input(request, github)
        self.assertEqual(packet["budget_usd"], 0.5)
        self.assertIn("Selected profile: focused security", packet["policy"])
        self.assertNotIn("Selected profile: focused security", review.policy("general"))
        with patch.object(review.Path, "read_text", return_value='{"profile":"security","budget_usd":0.5}'):
            self.assertEqual(review.settings(), ("security", 0.5))
        with patch.object(review.Path, "read_text", return_value='{"profile":"security","budget_usd":6}'):
            with self.assertRaises(review.ReviewError):
                review.settings()

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

    def test_completion_requires_an_accepted_submission_marker(self):
        for raw in ({**response(), "finish_reason": "stop"}, {**response(), "submission_calls": 0}):
            with self.assertRaisesRegex(review.ReviewError, "submit_review"):
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

    def test_costs_keep_cumulative_peak_and_cached_usage_separate(self):
        raw = response()
        raw.update(requests=39, peak_input_tokens=90000, compacted_tool_results=10, submission_calls=1)
        raw["usage"] = {"prompt_tokens": 2153652, "completion_tokens": 56870, "cached_prompt_tokens": 1000000}
        result = review.normalize(raw, REQUEST)
        self.assertEqual(result["estimated_cost_usd"], 0.71434)
        self.assertEqual(result["cache_adjusted_cost_usd"], 0.42034)
        self.assertEqual(result["peak_input_tokens"], 90000)
        self.assertIn("cumulative across requests", review.summary(result))
        raw["usage"]["cached_prompt_tokens"] = 3000000
        with self.assertRaisesRegex(review.ReviewError, "cached"):
            review.normalize(raw, REQUEST)

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
