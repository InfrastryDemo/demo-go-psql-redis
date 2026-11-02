"""Run against a fresh, disposable room: TEST_BASE_URL=http://localhost:8081 python3 tests/integration.py"""

import copy
import hashlib
import json
import os
import re
import time
import urllib.error
import urllib.request


class Browser:
    def __init__(self):
        self.token = ""

    def request(self, method, path, body=None, headers=None):
        request_headers = {
            "Content-Type": "application/json",
            "Cookie": "room_visitor=" + self.token,
            "X-CSRF-Token": self.token,
        }
        request_headers.update(headers or {})
        request = urllib.request.Request(
            os.environ.get("TEST_BASE_URL", "http://127.0.0.1:8081") + path,
            data=body.encode() if body is not None else None,
            headers=request_headers,
            method=method,
        )
        try:
            response = urllib.request.urlopen(request, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            text = response.read().decode()
            cookie = re.search(r"room_visitor=([a-f0-9]{64})", response.headers.get("Set-Cookie", ""))
            if cookie:
                self.token = cookie[1]
            data = json.loads(text) if "application/json" in response.headers.get("Content-Type", "") else text
            return response.status, data, response.headers

    def state(self):
        status, data, _ = self.request("GET", "/api/state")
        check(status == 200, "state loads")
        return data

    def post(self, body):
        return self.request("POST", "/api/messages", json.dumps({"body": body}))


checks = 0


def check(condition, description):
    global checks
    if not condition:
        raise AssertionError(description)
    checks += 1
    print("PASS:", description, flush=True)


a, b, c = Browser(), Browser(), Browser()
check(a.request("GET", "/health")[:2] == (200, {"status": "ok"}), "health checks the table and Redis")
check(a.request("GET", "/api/state")[0] == 401, "presence requires a session")
check(a.post("hello")[0] == 403, "posting requires a session")
status, home, headers = a.request("GET", "/")
check(status == 200 and "The common room" in home, "home loads")
check(len(a.token) == 64 and 'content="' + a.token + '"' in home, "cookie matches the CSRF token")
check("HttpOnly" in headers["Set-Cookie"] and "SameSite=Lax" in headers["Set-Cookie"], "cookie protection")
check(headers["Cache-Control"] == "no-store", "responses are not cached")
check("frame-ancestors 'none'" in headers["Content-Security-Policy"], "content security policy")
token = a.token
a.request("GET", "/")
check(a.token == token, "reload preserves identity")
for path, content_type in [("/assets/app.js", "text/javascript"), ("/assets/app.css", "text/css")]:
    status, content, headers = a.request("GET", path)
    check(status == 200 and content and content_type in headers["Content-Type"], path + " is served")
for path in ["/main.go", "/go.mod", "/.env", "/missing"]:
    check(a.request("GET", path)[0] == 404, path + " is not exposed")
check(a.state() == {"viewers": 1, "messages": []}, "first browser counts itself in an empty room (use a disposable stack)")
b.request("GET", "/")
check(b.state()["viewers"] == 2, "second browser sees two viewers including itself")
check(a.state()["viewers"] == 2, "first browser sees both viewers")
tab = copy.copy(a)
tab.request("GET", "/")
check(tab.state()["viewers"] == 2, "tabs share one identity")

for csrf in ["", "wrong"]:
    check(a.request("POST", "/api/messages", '{"body":"hello"}', {"X-CSRF-Token": csrf})[0] == 403, "invalid CSRF is rejected")
check(a.request("POST", "/api/messages", "body=hello", {"Content-Type": "application/x-www-form-urlencoded"})[0] == 415, "form submissions are rejected")
for raw in ["{", '{"body":"hello"} trailing']:
    check(a.request("POST", "/api/messages", raw)[0] == 400, "malformed JSON is rejected")
for raw in ['{"body":42}', "[]", "null", "{}"]:
    check(a.request("POST", "/api/messages", raw)[0] == 422, "a string body is required")
for body in ["", "   ", "\u2003", "a" * 1001, "🌱" * 1001, "null\0byte"]:
    check(a.post(body)[0] == 422, "invalid message is rejected")
check(a.request("POST", "/api/messages", "a" * 20000)[0] == 413, "request size is limited")
check(a.request("DELETE", "/api/messages")[0] == 405, "unsupported methods are rejected")

body = "Hello from the integration test 👋\nPostgreSQL keeps this message."
status, data, _ = a.post(body)
check(status == 201, "message is created")
message = data["message"]
check(message["body"] == body, "Unicode and line breaks survive")
check(message["author"] == "Visitor " + hashlib.sha256(a.token.encode()).hexdigest()[:6].upper(), "anonymous label matches the original")
check(a.token not in json.dumps(message), "public messages hide the token")
status, _, headers = a.post("too fast")
check(status == 429 and headers["Retry-After"] == "2", "posting cooldown is enforced")
check(message in b.state()["messages"], "another browser receives the persisted message")
literal = "<script>alert('xss')</script> '); DROP TABLE messages; --"
status, data, _ = b.post(literal)
check(status == 201 and data["message"]["body"] == literal, "HTML and SQL are stored as literal text")
c.request("GET", "/")
status, data, _ = c.post("🌱" * 1000)
check(status == 201, "1,000 Unicode characters are accepted, including escaped JSON")
check(a.state()["messages"][0]["id"] == data["message"]["id"], "newest messages appear first")

print("Waiting for idle viewers to expire…", flush=True)
deadline = time.monotonic() + 22
while time.monotonic() < deadline:
    time.sleep(2)
    state = a.state()
check(state["viewers"] == 1, "idle viewers expire while the active viewer still counts itself")
check(b.state()["viewers"] == 2, "an expired viewer can rejoin")
check(a.post("After the cooldown")[0] == 201, "posting resumes after cooldown")

# Separate sessions avoid the per-visitor cooldown while testing the history limit.
for index in range(101):
    browser = Browser()
    browser.request("GET", "/")
    check(browser.post("History check " + str(index))[0] == 201, "history message is saved")
messages = a.state()["messages"]
check(len(messages) == 100 and messages[0]["body"] == "History check 100", "only the latest 100 messages are returned")
check(all(left["id"] > right["id"] for left, right in zip(messages, messages[1:])), "history is in descending order")
print(f"\nAll {checks} checks passed. Remove the disposable stack and its volume when finished.")
