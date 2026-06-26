#!/usr/bin/env python3
"""Refresh OpenMessage Google account cookies from a local Chrome profile.

This is intentionally Linux/KWallet-specific. It updates only
auth_data.cookies in OpenMessage's session.json and never prints cookie values.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

from cryptography.hazmat.backends import default_backend
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes


REQUIRED_COOKIES = {
    ("messages.google.com", "OSID"),
    (".google.com", "SID"),
    (".google.com", "HSID"),
    (".google.com", "SSID"),
    (".google.com", "APISID"),
    (".google.com", "SAPISID"),
}
HOST_PRIORITY = {
    "messages.google.com": 0,
    ".google.com": 1,
    "accounts.google.com": 2,
}


def parse_args() -> argparse.Namespace:
    home = Path.home()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--profile",
        default=os.environ.get(
            "OPENMESSAGE_CHROME_PROFILE",
            str(home / ".config/google-chrome/Default"),
        ),
        help="Chrome profile directory containing Network/Cookies",
    )
    parser.add_argument(
        "--session",
        default=os.environ.get(
            "OPENMESSAGE_SESSION_PATH",
            str(home / ".local/share/openmessage/session.json"),
        ),
        help="OpenMessage session.json path",
    )
    parser.add_argument(
        "--wallet",
        default=os.environ.get("OPENMESSAGE_KWALLET", "kdewallet"),
        help="KWallet wallet name",
    )
    parser.add_argument(
        "--wallet-folder",
        default=os.environ.get("OPENMESSAGE_KWALLET_FOLDER", "Chrome Keys"),
        help="KWallet folder containing Chrome Safe Storage",
    )
    parser.add_argument(
        "--wallet-entry",
        default=os.environ.get("OPENMESSAGE_KWALLET_ENTRY", "Chrome Safe Storage"),
        help="KWallet password entry name",
    )
    parser.add_argument("--quiet", action="store_true", help="only print errors")
    parser.add_argument("--no-backup", action="store_true", help="do not write a session backup")
    return parser.parse_args()


def chrome_safe_storage_secret(args: argparse.Namespace) -> bytes:
    value = subprocess.check_output(
        [
            "kwallet-query",
            "--folder",
            args.wallet_folder,
            "--read-password",
            args.wallet_entry,
            args.wallet,
        ],
        text=False,
    ).rstrip(b"\n")
    if not value:
        raise RuntimeError("Chrome Safe Storage key was empty")
    return value


def derive_key(secret: bytes, iterations: int) -> bytes:
    return hashlib.pbkdf2_hmac("sha1", secret, b"saltysalt", iterations, 16)


def decrypt_cookie(encrypted_value: bytes, host: str, key: bytes) -> str | None:
    if not encrypted_value:
        return None
    encrypted_value = bytes(encrypted_value)
    if not (encrypted_value.startswith(b"v10") or encrypted_value.startswith(b"v11")):
        return None

    decryptor = Cipher(
        algorithms.AES(key),
        modes.CBC(b" " * 16),
        backend=default_backend(),
    ).decryptor()
    padded = decryptor.update(encrypted_value[3:]) + decryptor.finalize()
    pad_len = padded[-1]
    if pad_len < 1 or pad_len > 16:
        raise ValueError("invalid cookie padding")

    plain = padded[:-pad_len]
    host_hash = hashlib.sha256(host.encode()).digest()
    if plain.startswith(host_hash):
        plain = plain[32:]
    return plain.decode("utf-8")


def load_chrome_cookies(profile: Path, secret: bytes) -> tuple[dict[str, str], int, int, int]:
    cookie_db = profile / "Network" / "Cookies"
    if not cookie_db.exists():
        raise FileNotFoundError(f"Chrome cookie DB not found: {cookie_db}")

    rows = sqlite3.connect(f"file:{cookie_db}?mode=ro&immutable=1", uri=True).execute(
        """
        select host_key, name, encrypted_value, value
        from cookies
        where host_key in ('.google.com','messages.google.com','accounts.google.com')
           or host_key like '%.google.com'
        """
    ).fetchall()

    best: tuple[tuple[int, int, int], int, dict[str, tuple[str, str]], int, int, set[tuple[str, str]]] | None = None
    for iterations in (1, 1003):
        key = derive_key(secret, iterations)
        values: dict[str, tuple[str, str]] = {}
        ok = 0
        failed = 0
        for host, name, encrypted_value, plain_value in rows:
            value = plain_value or None
            if not value and encrypted_value:
                try:
                    value = decrypt_cookie(encrypted_value, host, key)
                except Exception:
                    failed += 1
                    continue
            if value is None:
                continue
            ok += 1
            previous = values.get(name)
            if previous is None or HOST_PRIORITY.get(host, 10) < HOST_PRIORITY.get(previous[0], 10):
                values[name] = (host, value)

        present = {
            (host, name)
            for name, (host, _value) in values.items()
            if (host, name) in REQUIRED_COOKIES
        }
        score = (len(present), ok, -failed)
        if best is None or score > best[0]:
            best = (score, iterations, values, ok, failed, present)

    if best is None:
        raise RuntimeError("no Google cookies found in Chrome profile")

    _score, iterations, values, ok, failed, present = best
    missing = sorted(REQUIRED_COOKIES - present)
    if missing:
        missing_text = ", ".join(f"{host}:{name}" for host, name in missing)
        raise RuntimeError(f"missing required cookies: {missing_text}")

    return {name: value for name, (_host, value) in values.items()}, iterations, ok, failed


def update_session(session_path: Path, cookies: dict[str, str], backup: bool) -> Path | None:
    if not session_path.exists():
        raise FileNotFoundError(f"OpenMessage session not found: {session_path}")

    backup_path = None
    if backup:
        backup_path = session_path.with_name(f"session.json.bak-{time.strftime('%Y%m%d-%H%M%S')}")
        shutil.copy2(session_path, backup_path)

    data = json.loads(session_path.read_text())
    data.setdefault("auth_data", {})["cookies"] = cookies

    tmp = session_path.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(data, separators=(",", ":")) + "\n")
    os.chmod(tmp, 0o600)
    tmp.replace(session_path)
    return backup_path


def main() -> int:
    args = parse_args()
    try:
        secret = chrome_safe_storage_secret(args)
        cookies, iterations, decrypted, failed = load_chrome_cookies(Path(args.profile), secret)
        backup_path = update_session(Path(args.session), cookies, backup=not args.no_backup)
    except Exception as exc:
        print(f"refresh_google_session_cookies: {exc}", file=sys.stderr)
        return 1

    if not args.quiet:
        print("session_cookie_refresh_ok")
        if backup_path:
            print(f"backup: {backup_path}")
        print(f"cookies_written: {len(cookies)}")
        print(f"decrypt_iterations: {iterations}")
        print(f"decrypted_cookies: {decrypted}")
        print(f"decrypt_failures: {failed}")
        print("required_present: APISID,HSID,OSID,SAPISID,SID,SSID")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
