#!/usr/bin/env python3
"""Exercise service preparation and recovery with an isolated compiled proxy."""

import json
import os
from pathlib import Path
import re
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request


def main():
    if sys.platform != "darwin" or len(sys.argv) != 2:
        raise SystemExit("Usage on macOS: python3 scripts/launchd/test.py ABSOLUTE_BINARY")
    binary = Path(sys.argv[1])
    if not binary.is_absolute() or not binary.is_file():
        raise SystemExit("Provide an absolute path to a compiled proxy binary")
    prepare = Path(__file__).resolve().with_name("prepare.sh")
    label = f"com.cliproxyapi.test.{os.getpid()}"
    domain = f"gui/{os.getuid()}"
    job = f"{domain}/{label}"
    subprocess.run(["launchctl", "print", domain], check=True, capture_output=True)
    if subprocess.run(["launchctl", "print", job], capture_output=True).returncode == 0:
        raise SystemExit("Refusing to touch an existing test job")
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]

    # Keep artifacts for diagnosis; the service is always unloaded after bootstrap.
    root = Path(tempfile.mkdtemp(prefix="cliproxy-launchd-test-"))
    config = root / "config & test.yaml"
    logs = root / "capture & logs"
    plist = root / "test.plist"
    key = secrets.token_hex(24)
    config.write_text(
        f"config-version: 8\nserver:\n  host: 127.0.0.1\n  port: {port}\n"
        f"access:\n  api-keys: [{key}]\noauth:\n  auth-dir: auths\n"
        "observability:\n  logs:\n    logging-to-file: true\n"
        "    logs-max-total-size-mb: 16\n    request-log: false\n"
    )
    config.chmod(0o600)
    command = ["sh", str(prepare), label, str(binary), str(config), str(root), str(logs), str(plist)]
    for index, invalid in [(2, "invalid label"), (3, "relative-binary")]:
        invalid_command = command.copy()
        invalid_command[index] = invalid
        assert subprocess.run(invalid_command, capture_output=True).returncode != 0
        assert not plist.exists()
    subprocess.run(command, check=True, capture_output=True)
    original = plist.read_bytes()
    assert subprocess.run(command, capture_output=True).returncode != 0
    assert plist.read_bytes() == original, "Preparer changed existing plist"
    assert plist.stat().st_mode & 0o777 == 0o600
    assert logs.stat().st_mode & 0o777 == 0o700
    arguments = subprocess.check_output(
        ["plutil", "-extract", "ProgramArguments", "json", "-o", "-", str(plist)]
    )
    assert json.loads(arguments) == [str(binary), "--config", str(config)]

    def ready(previous=None):
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            state = subprocess.check_output(["launchctl", "print", job], text=True)
            match = re.search(r"\bpid = (\d+)", state)
            current = int(match.group(1)) if match else None
            if current and current != previous:
                try:
                    request = urllib.request.Request(
                        f"http://127.0.0.1:{port}/v1/models",
                        headers={"Authorization": f"Bearer {key}"},
                    )
                    with urllib.request.urlopen(request, timeout=2) as response:
                        data = json.load(response)
                        if response.status == 200 and isinstance(data.get("data"), list):
                            return current
                except (OSError, ValueError):
                    pass
            time.sleep(0.25)
        raise RuntimeError("Isolated launchd job did not restore HTTP service")

    subprocess.run(["launchctl", "bootstrap", domain, str(plist)], check=True)
    try:
        first = ready()
        os.kill(first, signal.SIGABRT)
        second = ready(first)
        assert second != first
        assert "SIGABRT" in (logs / "stderr.log").read_text(), "Missing crash diagnostics"
        print("PASS: plist validation, overwrite refusal, private files, HTTP recovery, crash capture")
    finally:
        subprocess.run(["launchctl", "bootout", job], check=True)
    deadline = time.monotonic() + 15
    while subprocess.run(["launchctl", "print", job], capture_output=True).returncode == 0:
        if time.monotonic() >= deadline:
            raise RuntimeError("Test job remained loaded after bootout")
        time.sleep(0.25)
    print(f"Test job unloaded. Private artifacts retained at {root}")
    print("This test has no upstream credentials; model/tool verification is a separate deployment check.")


if __name__ == "__main__":
    main()
