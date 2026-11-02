import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

from install import install

spec = importlib.util.spec_from_file_location("review_package", Path(__file__).with_name("package.py"))
package = importlib.util.module_from_spec(spec)
spec.loader.exec_module(package)


class PackageTest(unittest.TestCase):
    def test_bundle_is_reproducible_and_install_is_explicit_and_idempotent(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            self.assertEqual(package.bundle(root / "one.zip"), package.bundle(root / "two.zip"))
            source, target = root / "bundle", root / "repo"
            target.mkdir()
            with zipfile.ZipFile(root / "one.zip") as archive:
                archive.extractall(source)
            self.assertTrue(install(source, target))
            self.assertEqual(list(target.iterdir()), [])
            paths = install(source, target, apply=True)
            self.assertIn(".github/workflows/ollama-review.yml", paths)
            self.assertIn(".github/workflows/ollama-review-diff.yml", paths)
            self.assertIn(".github/ollama-review-diff/review.py", paths)
            self.assertEqual(install(source, target, apply=True), [])
            self.assertFalse((target / "LICENSE").exists())

    def test_tampering_conflicts_and_symlinks_are_refused_before_writes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            package.bundle(root / "bundle.zip", "security")
            source, target = root / "bundle", root / "repo"
            target.mkdir()
            with zipfile.ZipFile(root / "bundle.zip") as archive:
                archive.extractall(source)
            settings = source / ".github/ollama-review/settings.json"
            original = settings.read_bytes()
            self.assertEqual(json.loads(original)["profile"], "security")
            settings.write_text("tampered")
            with self.assertRaisesRegex(ValueError, "Checksum"):
                install(source, target, apply=True)
            self.assertEqual(list(target.iterdir()), [])
            settings.write_bytes(original)
            conflict = target / "docs/ollama-review.md"
            conflict.parent.mkdir()
            conflict.write_text("keep my docs")
            with self.assertRaisesRegex(ValueError, "differs"):
                install(source, target, apply=True)
            self.assertFalse((target / ".github").exists())
            conflict.unlink()
            (target / ".github").symlink_to(root, target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "symlink"):
                install(source, target, apply=True)


if __name__ == "__main__":
    unittest.main()
