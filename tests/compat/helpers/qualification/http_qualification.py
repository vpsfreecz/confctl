"""Unrun, record-only original/candidate JSON and HTTP-codec qualification.

Uses only loopback sockets and explicitly supplied Ruby/test binaries. No
oracle output is inferred or regenerated. Raw process streams remain hex bytes.
"""

import argparse
import gzip
import json
import os
from pathlib import Path
import socketserver
import subprocess
import threading
import zlib


TEST = "remote::kexec_netboot::body::qualification::native_http_qualification_records_one_case_without_promoting_expectations"


def cases(family, syntax):
    if family == "json":
        return [(row["id"], "index", row["body"].encode(), [], "length")
                for row in json.loads(syntax.read_text())["cases"]]
    result = []
    for mode, payload in [("index", b'{"machines":[]}'), ("body", bytes(range(256)))]:
        gz = gzip.compress(payload, mtime=0)
        zz = zlib.compress(payload)
        corrupt = gz[:-1] + bytes([gz[-1] ^ 0x80])
        variants = [
            ("identity", payload, [], "length"),
            ("gzip", gz, [("Content-Encoding", "gzip")], "length"),
            ("deflate", zz, [("Content-Encoding", "deflate")], "length"),
            ("gzip-chunked", gz, [("Content-Encoding", "gzip")], "chunked"),
            ("deflate-chunked", zz, [("Content-Encoding", "deflate")], "chunked"),
            ("x-gzip", gz, [("Content-Encoding", "x-gzip")], "length"),
            ("mixed-case", gz, [("Content-Encoding", "GZip")], "length"),
            ("range-bypass", gz, [("Content-Encoding", "gzip"), ("Content-Range", "bytes 0-9/10")], "length"),
            ("unsupported-whole-header", gz, [("Content-Encoding", "gzip, br")], "length"),
            ("gzip-label-zlib-envelope", zz, [("Content-Encoding", "gzip")], "length"),
            ("deflate-label-gzip-envelope", gz, [("Content-Encoding", "deflate")], "length"),
            ("gzip-empty-wire", b"", [("Content-Encoding", "gzip")], "length"),
            ("gzip-empty-member", gzip.compress(b"", mtime=0), [("Content-Encoding", "gzip")], "length"),
            ("gzip-corrupt", corrupt, [("Content-Encoding", "gzip")], "length"),
            ("gzip-truncated", gz[:-5], [("Content-Encoding", "gzip")], "length"),
            ("gzip-trailing", gz + b"trailing", [("Content-Encoding", "gzip")], "length"),
            ("gzip-two-members", gz + gz, [("Content-Encoding", "gzip")], "length"),
            ("gzip-corrupt-second", gz + corrupt, [("Content-Encoding", "gzip")], "length"),
            ("zlib-trailing", zz + b"trailing", [("Content-Encoding", "deflate")], "length"),
            ("http-short-length", gz, [("Content-Encoding", "gzip")], "short-length"),
            ("http-short-chunk", gz, [("Content-Encoding", "gzip")], "short-chunk"),
        ]
        result.extend((name, mode, wire, headers, framing)
                      for name, wire, headers, framing in variants)
    return result


def capture(command, env, directory, label):
    result = subprocess.run(command, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=30, check=False)
    (directory / (label + ".stdout")).write_bytes(result.stdout)
    (directory / (label + ".stderr")).write_bytes(result.stderr)
    return {"argv": command, "native_exit": result.returncode if result.returncode >= 0 else None,
            "signal": -result.returncode if result.returncode < 0 else None,
            "stdout_hex": result.stdout.hex(), "stderr_hex": result.stderr.hex()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ruby", required=True, type=Path)
    parser.add_argument("--candidate-test-binary", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--family", choices=["json", "codec"], required=True)
    options = parser.parse_args()
    for executable in [options.ruby, options.candidate_test_binary]:
        if not executable.is_absolute() or not executable.is_file():
            parser.error("explicit existing absolute binaries are required")
    output = options.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output must be an empty owned directory")
    here = Path(__file__).resolve().parent
    syntax = here.parent / "candidate/json-syntax-cases.json"
    environment = os.environ.copy()
    # Loopback codec qualification isolates proxy/CA behavior, whose separate
    # native gates remain open. It does not change the product client policy.
    for key in list(environment):
        if key.lower() in {"http_proxy", "https_proxy", "all_proxy", "no_proxy"}:
            del environment[key]
    environment.update({"LC_ALL": "C", "LANG": "C"})
    for name, mode, wire, headers, framing in cases(options.family, syntax):
        directory = output / (mode + "-" + name)
        directory.mkdir()
        requests = []

        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                self.request.settimeout(5)
                request = bytearray()
                while not request.endswith(b"\r\n\r\n"):
                    chunk = self.request.recv(1)
                    if not chunk:
                        break
                    request.extend(chunk)
                    if len(request) > 65536:
                        raise RuntimeError("fixture request exceeded bound")
                requests.append(bytes(request).hex())
                response = [("Connection", "close")] + headers
                if framing in {"chunked", "short-chunk"}:
                    response.append(("Transfer-Encoding", "chunked"))
                    if framing == "chunked":
                        chunks = [wire[i:i + 3] for i in range(0, len(wire), 3)]
                        body = b"".join(format(len(chunk), "x").encode() + b"\r\n" + chunk + b"\r\n"
                                        for chunk in chunks) + b"0\r\n\r\n"
                    else:
                        body = format(len(wire) + 7, "x").encode() + b"\r\n" + wire
                else:
                    response.append(("Content-Length", str(len(wire) + (7 if framing == "short-length" else 0))))
                    body = wire
                head = b"HTTP/1.1 200 OK\r\n" + b"".join(
                    key.encode() + b": " + value.encode() + b"\r\n" for key, value in response) + b"\r\n"
                self.request.sendall(head + body)

        with socketserver.TCPServer(("127.0.0.1", 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
            thread.start()
            try:
                url = f"http://127.0.0.1:{server.server_address[1]}/machines.json"
                original = capture([str(options.ruby), str(here / "probe_http.rb"), url, mode],
                                   environment, directory, "original")
                split = len(requests)
                candidate_env = environment | {
                    "CONFCTL_TEST_HTTP_URL": url,
                    "CONFCTL_TEST_HTTP_MODE": mode,
                    "CONFCTL_TEST_HTTP_RECEIPT": str(directory / "candidate-adapter.json"),
                }
                candidate = capture([str(options.candidate_test_binary), TEST, "--ignored", "--exact", "--nocapture"],
                                    candidate_env, directory, "candidate")
                record = {"status": "OBSERVATIONS ONLY; no inferred parity verdict",
                          "case": name, "mode": mode, "wire_hex": wire.hex(),
                          "headers": headers, "framing": framing,
                          "original": original, "candidate": candidate,
                          "original_requests_hex": requests[:split], "candidate_requests_hex": requests[split:]}
                (directory / "record.json").write_text(json.dumps(record, indent=2) + "\n")
            finally:
                server.shutdown()
                thread.join()


if __name__ == "__main__":
    main()
