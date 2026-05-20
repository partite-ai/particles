# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""
Example particle exposing a few GitHub REST endpoints as tools — the
Python counterpart to ../github/Particlefile.ts.

Authentication: two alternatives — OAuth 2.0 or a personal access
token (PAT). At setup the user picks one; both substitute into
`Authorization: Bearer <token>`, so the handler doesn't branch on
which method is active.

    particle build           # walks `particle setup` interactively
    particle ping github-tools-py
    particle run  github-tools-py list_issues --owner=octocat --repo=hello-world
    particle run  github-tools-py create_issue --owner=me --repo=mine --title="Bug"
"""

import json
from particle import http, credentials
from particle.manifest import (
    Particle, Tool, Http,
    Credential, OAuth2, ApiKey, ApiKeyLocation,
)


def _gh(path: str, *, method: str = "GET", body=None):
    """Convenience wrapper: every tool sets the recommended Accept
    header and tags the request with the `github` credential so the
    host substitutes the bearer token at the wire boundary. Non-2xx
    responses bubble as a thrown error so the runtime returns a
    HandlerError with the API's message.

    `body` is JSON-encoded here (instead of in the caller) so handlers
    can pass a dict and get the right Content-Type without thinking
    about it.
    """
    headers = {"Accept": "application/vnd.github+json"}
    payload = None
    if body is not None:
        headers["Content-Type"] = "application/json"
        payload = json.dumps(body).encode("utf-8")
    res = http.fetch(
        f"https://api.github.com{path}",
        method=method,
        headers=headers,
        body=payload,
        credential_name="github",
    )
    if not res.ok:
        raise RuntimeError(f"GitHub API {res.status_code}: {res.text}")
    return res.json()


def _get_repo(args):
    r = _gh(f"/repos/{args['owner']}/{args['repo']}")
    return {
        "full_name":     r.get("full_name"),
        "description":   r.get("description"),
        "stars":         r.get("stargazers_count"),
        "open_issues":   r.get("open_issues_count"),
        "default_branch": r.get("default_branch"),
        "url":           r.get("html_url"),
    }


def _list_issues(args):
    state = args.get("state", "open")
    per_page = int(args.get("per_page", 30))
    issues = _gh(
        f"/repos/{args['owner']}/{args['repo']}/issues?state={state}&per_page={per_page}"
    )
    # GitHub's /issues endpoint also returns PRs (each carries a
    # `pull_request` field) — strip them out so the tool's name
    # matches its behavior.
    return [
        {
            "number": i.get("number"),
            "title":  i.get("title"),
            "state":  i.get("state"),
            "author": (i.get("user") or {}).get("login"),
            "url":    i.get("html_url"),
        }
        for i in issues
        if "pull_request" not in i
    ]


def _create_issue(args):
    body_text = args.get("body")
    payload = {"title": args["title"]}
    if body_text is not None:
        payload["body"] = body_text
    issue = _gh(
        f"/repos/{args['owner']}/{args['repo']}/issues",
        method="POST",
        body=payload,
    )
    return {"number": issue.get("number"), "url": issue.get("html_url")}


def _ping():
    """Probe the credential by hitting /user. Returns the
    authenticated login on success; degrades to "unhealthy" if
    GitHub rejects the token (refresh likely failed or the user
    revoked the grant).
    """
    method = credentials.get_configured_method("github")
    if method is None:
        return {"status": "unhealthy", "message": "no credential configured"}
    res = http.fetch("https://api.github.com/user", credential_name="github")
    if res.ok:
        login = res.json().get("login", "unknown")
        return {"status": "ok", "message": f"authenticated as {login} via {method}"}
    return {"status": "unhealthy", "message": f"GitHub /user returned {res.status_code}"}


particle = Particle(
    name="github-tools-py",
    description="Read repos and issues; open new issues. (Python edition.)",
    version="0.1.0",

    http=Http(allowed_hosts=["api.github.com"]),

    credentials={
        "github": Credential(
            # Substitution only fires on requests to api.github.com —
            # defense-in-depth: if the script ever planted the
            # placeholder in a non-GitHub request, it'd transmit
            # literally and the upstream would 401.
            hosts=["api.github.com"],
            required=True,
            methods={
                # OAuth: full account-level access. Best for
                # interactive users; the device-code flow makes it
                # work even without a local browser. The endpoints
                # are pinned in the manifest so setup never prompts
                # for them.
                "oauth": OAuth2(
                    description="Sign in to GitHub via OAuth",
                    flows=["authorization-code", "device-code"],
                    scopes=["repo", "read:user"],
                    authorization_url="https://github.com/login/oauth/authorize",
                    token_url="https://github.com/login/oauth/access_token",
                    device_auth_url="https://github.com/login/device/code",
                ),
                # PAT: a fine-grained or classic personal access
                # token. Best for CI / headless setups where a
                # browser-based OAuth round-trip is awkward.
                "pat": ApiKey(
                    description="Use a personal access token",
                    location=ApiKeyLocation(kind="auth-scheme", scheme="Bearer"),
                ),
            },
        ),
    },

    tools={
        "get_repo": Tool(
            description="Fetch a repository's metadata.",
            input_schema={
                "type": "object",
                "properties": {
                    "owner": {"type": "string", "description": "Owning user or organization."},
                    "repo":  {"type": "string", "description": "Repository name."},
                },
                "required": ["owner", "repo"],
            },
            handler=_get_repo,
        ),

        "list_issues": Tool(
            description="List issues for a repository.",
            input_schema={
                "type": "object",
                "properties": {
                    "owner":    {"type": "string"},
                    "repo":     {"type": "string"},
                    "state":    {
                        "type": "string",
                        "description": "Filter by state.",
                        "enum": ["open", "closed", "all"],
                        "default": "open",
                    },
                    "per_page": {
                        "type": "integer",
                        "description": "Max number of issues to return (1-100).",
                        "default": 30,
                    },
                },
                "required": ["owner", "repo"],
            },
            handler=_list_issues,
        ),

        "create_issue": Tool(
            description="Open a new issue on a repository.",
            input_schema={
                "type": "object",
                "properties": {
                    "owner": {"type": "string"},
                    "repo":  {"type": "string"},
                    "title": {"type": "string", "description": "Issue title."},
                    "body":  {"type": "string", "description": "Issue body in GitHub-flavored Markdown."},
                },
                "required": ["owner", "repo", "title"],
            },
            handler=_create_issue,
        ),
    },

    ping=_ping,
)
