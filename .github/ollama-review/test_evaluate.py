import csv
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import patch

from evaluate import score


FIXTURES = Path(__file__).with_name("evals")


class SecurityFixturesTest(unittest.TestCase):
    def test_known_defect_fixed_and_clean_control_have_the_claimed_behavior(self):
        # Execute only these authored synthetic fixtures, never PR-provided code.
        storage = types.ModuleType("storage")
        exec(compile((FIXTURES / "storage.py").read_text(), "storage.py", "exec"), storage.__dict__)
        for variant in ("baseline", "vulnerable", "fixed", "clean"):
            namespace = {}
            with self.subTest(variant=variant), patch.dict("sys.modules", {"storage": storage}):
                exec(compile((FIXTURES / variant / "api.py").read_text(), f"{variant}/api.py", "exec"), namespace)
                self.assertEqual(namespace["invoice"]("alice", "invoice-1"), "SYNTHETIC_INVOICE")
                if variant == "vulnerable":
                    self.assertEqual(namespace["invoice"]("bob", "invoice-1"), "SYNTHETIC_INVOICE")
                else:
                    with self.assertRaises(PermissionError):
                        namespace["invoice"]("bob", "invoice-1")

    def test_unlabeled_results_are_not_claimed_as_model_effectiveness(self):
        self.assertFalse(score(FIXTURES / "labels.csv")["passed"])
        with tempfile.TemporaryDirectory() as temp:
            labels = Path(temp) / "labels.csv"
            with labels.open("w", newline="") as output:
                writer = csv.writer(output)
                writer.writerow(["case", "status", "confirmed_expected", "unsupported_findings"])
                writer.writerows([["missing-owner-check", "complete", "yes", "0"],
                                 ["owner-check-restored", "complete", "no", "0"],
                                 ["unrelated-label-change", "complete", "no", "0"]])
            self.assertTrue(score(labels)["passed"])
            labels.write_text(labels.read_text().replace("owner-check-restored,complete,no", "owner-check-restored,complete,yes"))
            self.assertFalse(score(labels)["passed"])


if __name__ == "__main__":
    unittest.main()
