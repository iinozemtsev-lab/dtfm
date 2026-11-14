#!/usr/bin/env python3
"""Hermetic unit tests for scripts/install.sh using a local mock HTTP server."""

import hashlib
import http.server
import os
import platform
import shutil
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path


INSTALL_SH = Path(__file__).resolve().parent / "install.sh"
TEST_REPO = "iinozemtsev-lab/dtfm"
TEST_TAG = "v0.9.9"


def detect_os_arch():
    sys_name = platform.system()
    if sys_name == "Linux":
        os_name = "linux"
    elif sys_name == "Darwin":
        os_name = "darwin"
    else:
        raise RuntimeError(f"Unsupported test OS: {sys_name}")

    mach = platform.machine()
    if mach in ("x86_64", "amd64"):
        arch = "amd64"
    elif mach in ("aarch64", "arm64"):
        arch = "arm64"
    else:
        raise RuntimeError(f"Unsupported test arch: {mach}")
    return os_name, arch


class MockReleaseServer:
    def __init__(self, os_name: str, arch: str):
        self.os_name = os_name
        self.arch = arch
        self.requests = []
        self.corrupt_assets = set()
        self.files = self._build_assets()
        self._server = self._start()

    def _build_assets(self):
        mapping = {
            f"dtfm-{self.os_name}-{self.arch}": f"#!/bin/sh\necho 'dtfm {TEST_TAG}'\n",
            f"lmdtfy-{self.os_name}-{self.arch}": f"#!/bin/sh\necho 'lmdtfy {TEST_TAG}'\n",
            f"relay-{self.os_name}-{self.arch}": f"#!/bin/sh\necho 'dtfm-relay {TEST_TAG}'\n",
        }
        files = {}
        sums_lines = []
        for asset, script in mapping.items():
            data = script.encode("utf-8")
            files[asset] = data
            digest = hashlib.sha256(data).hexdigest()
            sums_lines.append(f"{digest}  {asset}\n")
        files["SHA256SUMS"] = "".join(sums_lines).encode("utf-8")
        return files

    def _start(self):
        parent = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, format, *args):
                return

            def _handle(self, send_body: bool):
                parent.requests.append((self.command, self.path))
                latest_path = f"/{TEST_REPO}/releases/latest"
                tag_path = f"/{TEST_REPO}/releases/tag/{TEST_TAG}"
                dl_prefix = f"/{TEST_REPO}/releases/download/{TEST_TAG}/"

                if self.path == latest_path:
                    self.send_response(302)
                    self.send_header("Location", tag_path)
                    self.end_headers()
                    return

                if self.path == tag_path:
                    self.send_response(200)
                    self.send_header("Content-Length", "2")
                    self.end_headers()
                    if send_body:
                        self.wfile.write(b"ok")
                    return

                if self.path.startswith(dl_prefix):
                    asset = self.path[len(dl_prefix):]
                    if asset in parent.files:
                        data = parent.files[asset]
                        if asset in parent.corrupt_assets:
                            data = b"#!/bin/sh\necho tampered\n"
                        self.send_response(200)
                        self.send_header("Content-Length", str(len(data)))
                        self.end_headers()
                        if send_body:
                            self.wfile.write(data)
                        return

                self.send_response(404)
                self.end_headers()

            def do_HEAD(self):
                self._handle(send_body=False)

            def do_GET(self):
                self._handle(send_body=True)

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        return server

    @property
    def url(self) -> str:
        host, port = self._server.server_address
        return f"http://{host}:{port}"

    def close(self):
        self._server.shutdown()
        self._server.server_close()


class InstallScriptTests(unittest.TestCase):
    def setUp(self):
        self.os_name, self.arch = detect_os_arch()
        self.server = MockReleaseServer(self.os_name, self.arch)
        self.tmp = tempfile.mkdtemp(prefix="dtfm-install-test-")

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmp, ignore_errors=True)

    def _base_env(self, extra=None):
        env = os.environ.copy()
        env["DTFM_GITHUB_URL"] = self.server.url
        env.pop("DTFM_INSTALL_DIR", None)
        env.pop("DTFM_VERSION", None)
        env.pop("DTFM_REPO", None)
        if extra:
            env.update(extra)
        return env

    def _run_install(self, args, env=None, stdin_bytes=None):
        cmd = ["sh", str(INSTALL_SH)] + args if stdin_bytes is None else ["sh", "-s", "--"] + args
        return subprocess.run(
            cmd,
            input=stdin_bytes,
            capture_output=True,
            text=stdin_bytes is None,
            env=env or self._base_env(),
            timeout=15,
        )

    def test_default_install_and_path_hint(self):
        dest = os.path.join(self.tmp, "bin")
        res = self._run_install(["--dir", dest])
        self.assertEqual(res.returncode, 0, msg=res.stderr)

        for binary in ("dtfm", "lmdtfy"):
            path = Path(dest) / binary
            self.assertTrue(path.is_file(), f"missing {binary}")
            out = subprocess.check_output([str(path), "--version"], text=True).strip()
            self.assertEqual(out, f"{binary} {TEST_TAG}")

        self.assertFalse((Path(dest) / "dtfm-relay").exists())
        self.assertIn(f'export PATH="{dest}:$PATH"', res.stdout)
        self.assertIn("gh attestation verify", res.stdout)

    def test_component_selection_and_relay_mapping(self):
        dest1 = os.path.join(self.tmp, "only-lmdtfy")
        res1 = self._run_install(["--dir", dest1, "lmdtfy"])
        self.assertEqual(res1.returncode, 0, msg=res1.stderr)
        self.assertTrue((Path(dest1) / "lmdtfy").is_file())
        self.assertFalse((Path(dest1) / "dtfm").exists())

        for comp in ("relay", "dtfm-relay"):
            dest_r = os.path.join(self.tmp, f"relay-{comp}")
            res_r = self._run_install(["--dir", dest_r, comp])
            self.assertEqual(res_r.returncode, 0, msg=res_r.stderr)
            relay_bin = Path(dest_r) / "dtfm-relay"
            self.assertTrue(relay_bin.is_file())
            self.assertFalse((Path(dest_r) / "relay").exists())
            out = subprocess.check_output([str(relay_bin), "--version"], text=True).strip()
            self.assertEqual(out, f"dtfm-relay {TEST_TAG}")

    def test_idempotent_rerun_and_force(self):
        dest = os.path.join(self.tmp, "idem")
        res1 = self._run_install(["--dir", dest, "--version", TEST_TAG, "lmdtfy"])
        self.assertEqual(res1.returncode, 0, msg=res1.stderr)

        self.server.requests.clear()
        res2 = self._run_install(["--dir", dest, "--version", TEST_TAG, "lmdtfy"])
        self.assertEqual(res2.returncode, 0, msg=res2.stderr)
        self.assertIn("already up to date", res2.stdout)
        self.assertEqual(self.server.requests, [])

        self.server.requests.clear()
        res3 = self._run_install(["--dir", dest, "--version", TEST_TAG, "--force", "lmdtfy"])
        self.assertEqual(res3.returncode, 0, msg=res3.stderr)
        self.assertTrue(any("SHA256SUMS" in p for _, p in self.server.requests))

    def test_sha256_mismatch_preserves_existing(self):
        dest = os.path.join(self.tmp, "mismatch")
        os.makedirs(dest, exist_ok=True)
        target = Path(dest) / "dtfm"
        target.write_text("existing-binary")
        target.chmod(0o755)

        asset = f"dtfm-{self.os_name}-{self.arch}"
        self.server.corrupt_assets.add(asset)

        res = self._run_install(["--dir", dest, "--version", TEST_TAG, "dtfm"])
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("SHA-256 mismatch", res.stderr)
        self.assertEqual(target.read_text(), "existing-binary")

    def test_symlink_guard(self):
        dest = os.path.join(self.tmp, "symlink")
        os.makedirs(dest, exist_ok=True)
        real_file = Path(self.tmp) / "real-lmdtfy"
        real_file.write_text("sentinel")
        link = Path(dest) / "lmdtfy"
        link.symlink_to(real_file)

        res_fail = self._run_install(["--dir", dest, "--version", TEST_TAG, "lmdtfy"])
        self.assertNotEqual(res_fail.returncode, 0)
        self.assertIn("symlink", res_fail.stderr)
        self.assertTrue(link.is_symlink())

        res_force = self._run_install(["--dir", dest, "--version", TEST_TAG, "--force", "lmdtfy"])
        self.assertEqual(res_force.returncode, 0, msg=res_force.stderr)
        self.assertFalse(link.is_symlink())
        self.assertTrue(link.is_file())
        out = subprocess.check_output([str(link), "--version"], text=True).strip()
        self.assertEqual(out, f"lmdtfy {TEST_TAG}")

    def test_invalid_version_rejected(self):
        dest = os.path.join(self.tmp, "badver")
        res = self._run_install(["--dir", dest, "--version", "../evil", "lmdtfy"])
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("Invalid release tag", res.stderr)
        self.assertEqual(self.server.requests, [])

        res_nl = self._run_install(
            ["--dir", dest, "lmdtfy"],
            env=self._base_env({"DTFM_VERSION": "v1.0.0\nevil"}),
        )
        self.assertNotEqual(res_nl.returncode, 0)
        self.assertIn("Invalid release tag", res_nl.stderr)

    def test_piped_execution_stdin_safe(self):
        dest = os.path.join(self.tmp, "piped")
        script_bytes = INSTALL_SH.read_bytes()
        res = self._run_install(["--dir", dest], stdin_bytes=script_bytes)
        self.assertEqual(res.returncode, 0, msg=res.stderr.decode("utf-8", errors="replace"))
        self.assertTrue((Path(dest) / "dtfm").is_file())
        self.assertTrue((Path(dest) / "lmdtfy").is_file())


if __name__ == "__main__":
    unittest.main()
