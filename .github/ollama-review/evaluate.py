#!/usr/bin/env python3
"""Score human-adjudicated synthetic review results. Never calls a model."""

import argparse
import csv
import json
from pathlib import Path


CASES = Path(__file__).with_name("evals") / "cases.json"


def score(labels):
    cases = json.loads(CASES.read_text())
    rows = {}
    with labels.open(newline="") as source:
        for row in csv.DictReader(source):
            if row["case"] in rows:
                raise ValueError("Duplicate case label")
            rows[row["case"]] = row
    if set(rows) != {case["id"] for case in cases}:
        raise ValueError("Labels must name each fixture exactly once")
    results = []
    for case in cases:
        row = rows[case["id"]]
        labeled = (row["status"] == "complete" and row["confirmed_expected"] in {"yes", "no"}
                   and row["unsupported_findings"].isdigit())
        passed = labeled and (row["confirmed_expected"] == "yes") == case["expected_bug"] and int(row["unsupported_findings"]) == 0
        results.append({"case": case["id"], "labeled": labeled, "passed": passed})
    return {"passed": all(case["passed"] for case in results), "cases": results,
            "scope": "Human labels for synthetic cases only; this does not establish general security effectiveness."}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--labels", type=Path, required=True)
    args = parser.parse_args()
    result = score(args.labels)
    print(json.dumps(result, indent=2))
    raise SystemExit(0 if result["passed"] else 1)
