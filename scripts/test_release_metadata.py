"""Exercise release provenance with disposable repositories and real git tags."""

from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest

from release_metadata import resolve_release


class ReleaseMetadataTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        self.git("init", "--quiet", "--initial-branch=main")
        self.git("config", "user.name", "Release Test")
        self.git("config", "user.email", "release-test@example.invalid")
        self.plist = self.repo / "macos/OpenMessage/Sources/Info.plist"
        self.plist.parent.mkdir(parents=True)
        self.write_version("0.3.0")
        self.commit()
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], stderr=subprocess.PIPE).decode().strip()

    def write_version(self, version, build="19"):
        self.plist.write_bytes(plistlib.dumps({"CFBundleShortVersionString": version, "CFBundleVersion": build}))

    def commit(self):
        self.git("add", ".")
        self.git("commit", "--quiet", "-m", "Test release")

    def test_lightweight_and_annotated_tags_resolve_to_commit(self):
        for annotated in (False, True):
            with self.subTest(annotated=annotated):
                flags = ["-a", "-m", "Release"] if annotated else []
                self.git("tag", *flags, "v0.3.0")
                self.assertEqual(resolve_release(self.repo, "v0.3.0"), {
                    "tag": "v0.3.0", "commit": self.git("rev-parse", "HEAD"),
                    "version": "0.3.0", "build": "19",
                })
                self.git("tag", "-d", "v0.3.0")

    def test_requested_tag_wins_over_checked_out_branch_and_working_tree(self):
        self.git("tag", "v0.3.0")
        tagged = self.git("rev-parse", "HEAD")
        self.write_version("0.4.0")
        self.commit()
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.write_version("9.9.9")
        self.assertEqual(resolve_release(self.repo, "v0.3.0")["commit"], tagged)

    def test_cli_writes_the_validated_commit_to_github_outputs(self):
        self.git("tag", "v0.3.0")
        output = self.repo / "github-output"
        result = subprocess.run([
            "python3", str(Path(__file__).with_name("release_metadata.py")),
            "--repo", str(self.repo), "--tag", "v0.3.0", "--github-output", str(output),
        ], check=True, capture_output=True, text=True)
        self.assertEqual(output.read_text(), result.stdout)
        values = dict(line.split("=", 1) for line in output.read_text().splitlines())
        self.assertEqual(values["commit"], self.git("rev-parse", "HEAD"))
        self.assertEqual(values["tag"], "v0.3.0")

    def test_unmerged_commit_is_rejected(self):
        self.git("checkout", "-b", "unmerged")
        self.write_version("0.3.0", "20")
        self.commit()
        self.git("tag", "v0.3.0")
        with self.assertRaisesRegex(ValueError, "merged into main"):
            resolve_release(self.repo, "v0.3.0")

    def test_tag_and_bundle_version_must_match(self):
        self.git("tag", "v0.4.0")
        with self.assertRaisesRegex(ValueError, "does not match"):
            resolve_release(self.repo, "v0.4.0")

    def test_missing_tag_or_branch_named_like_tag_is_rejected(self):
        self.git("branch", "v0.3.0")
        with self.assertRaisesRegex(ValueError, "must exist"):
            resolve_release(self.repo, "v0.3.0")

    def test_invalid_tags_are_rejected(self):
        for tag in ("main", "v0.3.0\ncommit=bad", "v0.3.0;echo bad", "v0.3.0-rc.1", "v00.3.0"):
            with self.subTest(tag=tag), self.assertRaisesRegex(ValueError, "stable version"):
                resolve_release(self.repo, tag)

    def test_invalid_build_is_rejected(self):
        self.write_version("0.3.0", "dev")
        self.commit()
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.git("tag", "v0.3.0")
        with self.assertRaisesRegex(ValueError, "positive integer"):
            resolve_release(self.repo, "v0.3.0")


if __name__ == "__main__":
    unittest.main()
