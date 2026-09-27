import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from release_version import (
    PINNED_FILES,
    ROOT,
    VERSION,
    defaults_already_prepared,
    latest_version,
    next_version,
    update_defaults,
)


class ReleaseVersionTest(unittest.TestCase):
    def test_current_defaults_agree_with_image_build_default(self):
        build_script = (ROOT / "deploy/images/build.sh").read_text(encoding="utf-8")
        # VERSION is anchored for tags, not embedded shell lines; extract the
        # maintained image default explicitly before comparing other files.
        current = build_script.split('default_tag="${QUOIN_IMAGE_TAG:-', 1)[1].split('}"', 1)[0]
        self.assertRegex(current, VERSION)
        for name, expected in PINNED_FILES.items():
            with self.subTest(name=name):
                self.assertEqual((ROOT / name).read_text(encoding="utf-8").count(current), expected)

    def test_latest_version_uses_semver_not_lexical_order(self):
        self.assertEqual(latest_version(["v0.1.9", "v0.1.10", "v0.2.0", "draft"]), "v0.2.0")
        with self.assertRaisesRegex(ValueError, "no reachable"):
            latest_version(["v01.2.0", "draft"])

    def test_fix_and_feature_bumps(self):
        self.assertEqual(next_version("v0.1.2", "fix"), "v0.1.3")
        self.assertEqual(next_version("v0.1.2", "feature"), "v0.2.0")
        self.assertEqual(next_version("v1.9.8", "feature"), "v1.10.0")

    def test_updates_only_pinned_files_after_validating_all_counts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, count in PINNED_FILES.items():
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(("v0.1.2\n" * count), encoding="utf-8")
            historical = root / "history.txt"
            historical.write_text("v0.1.2", encoding="utf-8")

            update_defaults(root, "v0.1.2", "v0.1.3")
            self.assertTrue(defaults_already_prepared(root, "v0.1.2", "v0.1.3"))
            self.assertEqual(historical.read_text(encoding="utf-8"), "v0.1.2")
            for name, count in PINNED_FILES.items():
                self.assertEqual((root / name).read_text(encoding="utf-8"), "v0.1.3\n" * count)

    def test_drift_fails_without_partial_writes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, count in PINNED_FILES.items():
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("v0.1.2\n" * count, encoding="utf-8")
            (root / "docs/getting-started.md").write_text("unexpected", encoding="utf-8")
            self.assertFalse(defaults_already_prepared(root, "v0.1.2", "v0.1.3"))

            with self.assertRaisesRegex(ValueError, "docs/getting-started.md"):
                update_defaults(root, "v0.1.2", "v0.1.3")
            self.assertEqual((root / "Makefile").read_text(encoding="utf-8"), "v0.1.2\n")

    def test_release_prepare_can_resume_untagged_commit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, count in PINNED_FILES.items():
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("v0.1.2\n" * count, encoding="utf-8")
            script = root / "scripts/release_version.py"
            script.parent.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/release_version.py", script)

            def git(*args):
                return subprocess.check_output(["git", *args], cwd=root, text=True).strip()

            git("init", "-q")
            git("config", "user.name", "Release test")
            git("config", "user.email", "test@example.invalid")
            git("add", ".")
            git("commit", "-qm", "existing release")
            git("tag", "-a", "v0.1.2", "-m", "Quoin v0.1.2")
            git("commit", "--allow-empty", "-qm", "fix: demo")
            command = [sys.executable, "-B", str(script), "fix"]
            self.assertEqual(subprocess.check_output(command, cwd=root, text=True).strip(), "v0.1.3")
            git("add", ".")
            git("commit", "-qm", "release: prepare v0.1.3")
            self.assertEqual(subprocess.check_output(command, cwd=root, text=True).strip(), "v0.1.3")
            self.assertEqual(git("status", "--porcelain"), "")
            git("tag", "-a", "v0.1.3", "-m", "Quoin v0.1.3")
            resumed = subprocess.run(command, cwd=root, capture_output=True, text=True, check=False)
            self.assertNotEqual(resumed.returncode, 0)
            self.assertIn("nothing new to release", resumed.stderr)


if __name__ == "__main__":
    unittest.main()
