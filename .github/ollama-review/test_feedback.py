import copy
import unittest

from feedback import body, finish, post_inline, update
from review import ReviewError
from test_review import BASE, HEAD, PATCH, PR, REQUEST


def status(run=100, attempt=1, owner="github-actions[bot]"):
    return {"id": 9, "body": f"<!-- ollama-review-status run={run} attempt={attempt} -->\nold",
            "user": {"login": owner, "type": "Bot" if owner.endswith("[bot]") else "User"}}


def result():
    return {"status": "complete", "base": BASE, "head": HEAD, "requests": 1,
            "findings": [{"path": "app.py", "start_line": 3, "end_line": 3, "body": "This branch loses data."}],
            "usage": {"prompt_tokens": 1000, "completion_tokens": 100}, "context_limited": []}


class FakeAPI:
    repository = "owner/repo"

    def __init__(self):
        self.comments = [status()]
        self.inline = []
        self.reviews = []
        self.pr = copy.deepcopy(PR)
        self.writes = []
        self.fail_inline = False
        self.comment_pages = None

    def get(self, path):
        if path.startswith("issues/7/comments"):
            if self.comment_pages:
                page = int(path.split("page=")[-1])
                return self.comment_pages.get(page, [])
            return copy.deepcopy(self.comments)
        if path.startswith("pulls/7/reviews"):
            return self.reviews
        if path.startswith("pulls/7/comments"):
            return self.inline
        raise AssertionError(path)

    def pull(self, number):
        return self.pr

    def files(self, number, expected):
        return [{"filename": "app.py", "patch": PATCH}]

    def request(self, method, path, payload):
        if path == "pulls/7/reviews" and self.fail_inline:
            raise ReviewError("HTTP 422")
        self.writes.append((method, path, payload))
        if path == "issues/comments/9":
            self.comments[0]["body"] = payload["body"]
        elif path == "issues/7/comments":
            self.comments.append({"id": 10, **payload, "user": {"login": "github-actions[bot]", "type": "Bot"}})


class FeedbackTest(unittest.TestCase):
    def test_acknowledges_and_updates_the_same_owned_comment(self):
        api = FakeAPI()
        self.assertTrue(update(api, REQUEST, "queued", 101, 1))
        self.assertTrue(update(api, REQUEST, "running", 101, 1))
        self.assertEqual([path for _, path, _ in api.writes], ["issues/comments/9", "issues/comments/9"])
        self.assertIn("pipeline has started", api.writes[-1][2]["body"])

    def test_finds_comment_on_later_pages_and_never_edits_a_user_comment(self):
        api = FakeAPI()
        fake = status(owner="someone")
        api.comments = [fake]
        self.assertTrue(update(api, REQUEST, "queued", 101, 1))
        self.assertEqual(api.writes[0][1], "issues/7/comments")
        api = FakeAPI()
        api.comment_pages = {1: [fake] * 100, 2: [status()]}
        self.assertTrue(update(api, REQUEST, "queued", 101, 1))
        self.assertEqual(api.writes[-1][1], "issues/comments/9")

    def test_old_run_cannot_overwrite_new_status_or_publish_inline(self):
        api = FakeAPI()
        api.comments = [status(102)]
        self.assertFalse(update(api, REQUEST, "queued", 101, 1))
        self.assertFalse(finish(api, REQUEST, result(), "success", 100, 1))
        self.assertEqual(api.writes, [])

    def test_success_posts_inline_at_reviewed_head_and_updates_summary(self):
        api = FakeAPI()
        self.assertTrue(finish(api, REQUEST, result(), "success", 100, 1))
        review = api.writes[0][2]
        self.assertEqual(review["commit_id"], HEAD)
        self.assertEqual(review["event"], "COMMENT")
        self.assertEqual(review["comments"][0]["line"], 3)
        self.assertEqual(review["comments"][0]["side"], "RIGHT")
        self.assertIn("Complete", api.writes[-1][2]["body"])
        self.assertIn("loses data", api.writes[-1][2]["body"])

    def test_stale_invalid_and_failed_reviews_never_publish_findings(self):
        for kind in ("stale", "invalid", "failure", "cancelled"):
            api, output, job = FakeAPI(), result(), "success"
            if kind == "stale":
                api.pr["head"]["sha"] = "c" * 40
            elif kind == "invalid":
                output["base"] = "c" * 40
            elif kind == "failure":
                job, output = "failure", {"reason": "Inference failed."}
            else:
                job = "cancelled"
            with self.subTest(kind=kind):
                self.assertTrue(finish(api, REQUEST, output, job, 100, 1))
                self.assertTrue(all(path == "issues/comments/9" for _, path, _ in api.writes))
                self.assertNotIn("loses data", api.writes[-1][2]["body"])

    def test_existing_inline_or_rejected_location_falls_back_to_summary(self):
        api = FakeAPI()
        api.inline = [{"user": {"login": "github-actions[bot]", "type": "Bot"},
                       "commit_id": HEAD, "path": "app.py", "line": 3,
                       "body": "<!-- ollama-review-finding earlier -->\nAlready reported"}]
        self.assertTrue(finish(api, REQUEST, result(), "success", 100, 1))
        self.assertEqual(len(api.writes), 1)
        api = FakeAPI()
        api.fail_inline = True
        self.assertTrue(finish(api, REQUEST, result(), "success", 100, 1))
        self.assertIn("Inline feedback could not be confirmed", api.writes[-1][2]["body"])
        self.assertIn("loses data", api.writes[-1][2]["body"])

    def test_model_text_cannot_embed_images_html_or_notify_people(self):
        output = result()
        output["findings"][0]["body"] = "<img src=x> ![x](https://example.com) @team"
        output.update(estimated_cost_usd=0.1, omitted=[])
        text = body(REQUEST, "complete", 100, 1, output)
        self.assertNotIn("<img", text)
        self.assertNotIn(" @team", text)
        self.assertIn("\\![x]", text)


if __name__ == "__main__":
    unittest.main()
