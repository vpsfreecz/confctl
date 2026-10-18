"""Owned mock HaveAPI transport; no credential values enter its request log."""

from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import threading
from urllib.parse import parse_qs, urlsplit


def parameter(kind="String", **values):
    return dict(type=kind, validators={}, **values)


def action(path, method="GET", namespace="item", params=None, output=None, layout="object"):
    return dict(path=path, help=path, method=method, aliases=[], blocking=False,
                input=dict(layout="object", namespace=namespace, parameters=params or {}),
                output=dict(layout=layout, namespace=namespace, parameters=output or {}, format={}),
                meta={})


def resource(show):
    return dict(resources={}, actions=dict(show=show))


def description():
    scalar = parameter()
    reference = lambda name: parameter("Resource", resource=[name], value_id="id", value_label="id")
    environment = action("/environments/{id}", namespace="environment", output=dict(domain=scalar))
    location = action("/locations/{id}", namespace="location",
                      output=dict(domain=scalar, environment=reference("environment")))
    node = action("/nodes/{id}", namespace="node",
                  output=dict(id=parameter("Integer"), name=scalar, domain_name=scalar, location=reference("location")))
    vps = action("/vps/{id}", namespace="vps", output=dict(node=reference("node")))
    network = action("/networks", namespace="networks", layout="object_list",
                     output=dict(ip_version=parameter("Integer"), address=scalar, prefix=parameter("Integer")))
    request = action("/auth/token", "POST", "token",
                     params=dict(user=scalar, password=parameter(protected=True), lifetime=scalar,
                                 interval=parameter("Integer")),
                     output=dict(complete=parameter("Boolean"), next_action=scalar, token=scalar))
    challenge = action("/auth/challenge", "POST", "token",
                       params=dict(token=scalar, code=parameter(label="Verification code", protected=True)),
                       output=dict(complete=parameter("Boolean"), token=scalar, valid_to=scalar))
    return dict(authentication=dict(token=dict(http_header="X-Fixture-Token", query_parameter="token",
                                               resources=dict(token=dict(actions=dict(request=request, challenge=challenge))))),
                resources=dict(environment=resource(environment), location=resource(location), node=resource(node),
                               vps=resource(vps), network=dict(resources={}, actions=dict(list=network))))


class MockAPI:
    def __init__(self, mode):
        self.mode = mode
        self.events = []
        self.errors = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def reply(self, body, code=200, **headers):
                data = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                for key, value in headers.items():
                    self.send_header(key, value)
                self.end_headers()
                self.wfile.write(data)

            def do_OPTIONS(self):
                parsed = urlsplit(self.path)
                owner.events.append(dict(method="OPTIONS", path=parsed.path, query_keys=sorted(parse_qs(parsed.query))))
                if parsed.path != "/":
                    owner.errors.append("unexpected description path")
                self.reply(dict(version="2.0", response=description()))

            def do_POST(self):
                parsed = urlsplit(self.path)
                data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                token = data.get("token", {})
                # Record keys and nonsecret lifetime policy only. Assert synthetic
                # credential values in memory, never include them in errors.
                owner.events.append(dict(method="POST", path=parsed.path, keys=sorted(token),
                                         lifetime=token.get("lifetime"), interval=token.get("interval")))
                if parsed.path == "/auth/token":
                    if token.get("lifetime") != "fixed" or token.get("interval") != 60:
                        owner.errors.append("incorrect token lifetime policy")
                    if owner.mode == "api-auth-error":
                        self.reply(dict(status=False, message="fixture authentication rejected", errors={}))
                    else:
                        self.reply(dict(status=True, response=dict(token=dict(complete=False, next_action="challenge", token="fixture-step"))))
                elif parsed.path == "/auth/challenge":
                    if "code" not in token:
                        owner.errors.append("missing challenge field")
                    self.reply(dict(status=True, response=dict(token=dict(complete=True, token="fixture-session", valid_to=None))))
                else:
                    owner.errors.append("unexpected POST path")
                    self.reply({}, 500)

            def do_GET(self):
                parsed = urlsplit(self.path)
                owner.events.append(dict(method="GET", path=parsed.path, query=parse_qs(parsed.query),
                                         authenticated=self.headers.get("X-Fixture-Token") == "fixture-session"))
                if owner.mode == "api-redirect":
                    self.reply({}, 302, Location="https://api.vpsfree.cz/forbidden")
                elif parsed.path == "/networks":
                    self.reply(dict(status=True, response=dict(networks=[
                        dict(ip_version=4, address="192.0.2.0", prefix=24),
                        dict(ip_version=6, address="2001:db8::", prefix=32)], _meta=dict(total_count=2))))
                elif parsed.path == "/vps/1":
                    resolved = dict(resolved=True)
                    environment = dict(domain="example.invalid", _meta=resolved)
                    location = dict(domain="test", environment=environment, _meta=resolved)
                    node = dict(id=7, name="node", domain_name="node.test", location=location, _meta=resolved)
                    self.reply(dict(status=True, response=dict(vps=dict(node=node))))
                else:
                    owner.errors.append("unexpected GET path")
                    self.reply({}, 500)

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever)

    def start(self):
        self.thread.start()
        return "http://127.0.0.1:" + str(self.server.server_port)

    def stop(self):
        self.server.shutdown()
        self.thread.join()
        self.server.server_close()
