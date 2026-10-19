#!/usr/bin/env python3
"""Exercise the dedicated localhost quickstart; never accepts remote endpoints."""
import json
import time
import urllib.error
import urllib.request

ADMIN = "http://localhost:13333/api"
PROXY = "http://localhost:18080"
ADMIN_HEADERS = {"X-Admin-Key": "local-demo-admin-key"}


def request(url, body=None, headers=None, expected=200):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers={
        "Content-Type": "application/json", **(headers or {})})
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            status, payload = response.status, response.read()
    except urllib.error.HTTPError as error:
        status, payload = error.code, error.read()
    if status != expected:
        raise RuntimeError(f"{req.method} {url}: expected {expected}, got {status}: {payload[:200]!r}")
    return json.loads(payload) if payload else None


def main():
    for attempt in range(30):
        try:
            request(PROXY + "/readyz")
            request(ADMIN + "/tenants", headers=ADMIN_HEADERS)
            break
        except (OSError, RuntimeError):
            if attempt == 29:
                raise
            time.sleep(2)

    request(ADMIN + "/tenants", headers={"X-Admin-Key": "invalid-demo-key"}, expected=401)
    request(PROXY + "/proxy/v1/models", expected=401)
    tenant = request(ADMIN + "/tenants", {"name": "Quickstart smoke test"}, ADMIN_HEADERS, 201)
    user_url = ADMIN + "/tenants/" + tenant["id"] + "/users"
    user = request(user_url, {"email": "demo@example.com", "display_name": "Demo User"}, ADMIN_HEADERS, 201)
    key = request(user_url + "/" + user["id"] + "/keys", {"scopes": ["llm:invoke"]}, ADMIN_HEADERS, 201)
    headers = {"Authorization": "Bearer " + key["key"], "Cookie": "synthetic-client-cookie=must-not-reach-upstream"}
    catalog = request(PROXY + "/proxy/v1/models", headers=headers)
    if "demo-chat" not in json.dumps(catalog):
        raise RuntimeError("Local demo model missing from discovery")
    completion = request(PROXY + "/openai/v1/chat/completions", {
        "model": "demo-chat", "messages": [{"role": "user", "content": "Hello!"}]
    }, headers)
    expected = "Hello from the local PiProxy demo."
    if completion["choices"][0]["message"]["content"] != expected:
        raise RuntimeError("Unexpected mock response")
    if completion["usage"]["total_tokens"] != 17:
        raise RuntimeError("Mock token usage not preserved")
    print(json.dumps({"status": "passed", "checks": ["readiness", "invalid admin key rejected",
        "unauthenticated proxy rejected", "tenant/user/key onboarding", "model discovery",
        "proxied mock completion", "upstream credential injection", "client cookie stripped"],
        "completion": expected, "upstream_tokens": 17}, indent=2))


if __name__ == "__main__":
    main()
