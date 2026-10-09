#!/usr/bin/env python3
"""Resolve a release tag to an immutable, mainline commit before building."""

import argparse
from pathlib import Path
import plistlib
import re
import subprocess


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args], stderr=subprocess.PIPE)


def resolve_release(repo, tag, main_ref="refs/remotes/origin/main"):
    if not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", tag):
        raise ValueError("release tag must be a stable version such as v0.3.0")
    try:
        commit = git(repo, "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}").decode().strip()
        git(repo, "merge-base", "--is-ancestor", commit, main_ref)
        plist = plistlib.loads(git(repo, "show", f"{commit}:macos/OpenMessage/Sources/Info.plist"))
    except subprocess.CalledProcessError as exc:
        raise ValueError("release tag must exist and point to a commit merged into main") from exc
    if plist.get("CFBundleShortVersionString") != tag[1:]:
        raise ValueError("release tag does not match CFBundleShortVersionString at the tagged commit")
    build = str(plist.get("CFBundleVersion", ""))
    if not re.fullmatch(r"[1-9][0-9]*", build):
        raise ValueError("CFBundleVersion must be a positive integer")
    return {"tag": tag, "commit": commit, "version": tag[1:], "build": build}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--repo", type=Path, default=Path.cwd())
    parser.add_argument("--github-output", type=Path)
    args = parser.parse_args()
    try:
        metadata = resolve_release(args.repo, args.tag)
    except (ValueError, plistlib.InvalidFileException) as exc:
        parser.exit(1, f"Release validation failed: {exc}\n")
    output = "".join(f"{key}={value}\n" for key, value in metadata.items())
    print(output, end="")
    if args.github_output:
        with args.github_output.open("a") as target:
            target.write(output)


if __name__ == "__main__":
    main()
