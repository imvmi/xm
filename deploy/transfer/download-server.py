#!/usr/bin/env python3
"""Serve one private, streaming ZIP download without writing an archive to disk."""
import argparse
import hmac
import os
import pathlib
import secrets
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


ROOT = pathlib.Path(__file__).resolve().parent


def download_token():
    path = ROOT / ".download-token"
    try:
        with path.open("x", encoding="ascii") as stream:
            os.chmod(path, 0o600)
            stream.write(secrets.token_urlsafe(32))
    except FileExistsError:
        pass
    token = path.read_text(encoding="ascii").strip()
    if len(token) < 40:
        raise RuntimeError("Invalid download token")
    return token


def source_ready():
    state = subprocess.check_output(
        ["systemctl", "--user", "show", "xm.service", "--property=ActiveState", "--value"],
        text=True,
        timeout=10,
    ).strip()
    return state == "inactive" and (ROOT / "app" / "manifest.json").is_file()


class DownloadServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, token, command, ready):
        super().__init__(address, DownloadHandler)
        self.download_path = f"/download/{token}/xm-windows-full.zip"
        self.command = command
        self.ready = ready
        self.slot = threading.Lock()


class DownloadHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def setup(self):
        super().setup()
        self.connection.settimeout(120)

    def log_message(self, format_string, *args):
        # Request URLs contain the access token. Never put them in access logs.
        pass

    def message(self, status, text):
        payload = (text + "\n").encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        if self.command != "HEAD":
            self.wfile.write(payload)

    def authorized(self):
        if not hmac.compare_digest(
            self.path.encode("utf-8"), self.server.download_path.encode("utf-8")
        ):
            self.message(404, "Not found")
            return False
        return True

    def serve(self, head=False):
        if not self.authorized():
            return
        if self.headers.get("Range"):
            self.message(416, "This streaming ZIP cannot resume. Start a new download.")
            return
        if not self.server.slot.acquire(blocking=False):
            self.message(429, "Another download is active. Try again later.")
            return
        process = None
        try:
            try:
                ready = self.server.ready()
            except (OSError, subprocess.SubprocessError):
                ready = False
            if not ready:
                self.message(503, "Source service must be stopped and the package ready.")
                return
            if not head:
                try:
                    process = subprocess.Popen(
                        self.server.command,
                        stdin=subprocess.DEVNULL,
                        stdout=subprocess.PIPE,
                        # Only stream-archive's fixed status/error messages go to the journal.
                        stderr=None,
                        bufsize=1024 * 1024,
                    )
                except OSError:
                    self.message(503, "ZIP generator could not start.")
                    return
            self.send_response(200)
            self.send_header("Content-Type", "application/zip")
            self.send_header(
                "Content-Disposition", 'attachment; filename="xm-windows-full.zip"'
            )
            self.send_header("Cache-Control", "private, no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Referrer-Policy", "no-referrer")
            self.send_header("Accept-Ranges", "none")
            self.send_header("Transfer-Encoding", "chunked")
            self.send_header("Connection", "close")
            self.end_headers()
            self.close_connection = True
            if head:
                return
            while chunk := process.stdout.read(1024 * 1024):
                self.wfile.write(f"{len(chunk):X}\r\n".encode("ascii"))
                self.wfile.write(chunk)
                self.wfile.write(b"\r\n")
            if process.wait() != 0:
                # Omit the terminating chunk so clients detect an incomplete download.
                print("ZIP generation failed; download is incomplete.", file=sys.stderr)
                return
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
            print("HTTP ZIP download completed.", file=sys.stderr)
        except (BrokenPipeError, ConnectionError, TimeoutError):
            print("Download disconnected; ZIP generation stopped.", file=sys.stderr)
        except OSError:
            print("Download I/O error; transfer incomplete.", file=sys.stderr)
        finally:
            if process is not None:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                process.stdout.close()
            self.server.slot.release()

    def do_HEAD(self):
        self.serve(head=True)

    def do_GET(self):
        self.serve()

    def do_POST(self):
        self.message(405, "Method not allowed")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", default=3000, type=int)
    args = parser.parse_args()
    os.umask(0o077)
    token = download_token()
    server = DownloadServer(
        (args.host, args.port),
        token,
        [sys.executable, "-u", str(ROOT / "stream-archive.py")],
        source_ready,
    )
    # Keep the URL out of the journal; this private file is for the operator.
    (ROOT / ".download-path").write_text(server.download_path + "\n", encoding="ascii")
    print(f"XM download server listening on {args.host}:{args.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
