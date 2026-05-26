"""
urllib (stdlib) auto-hook — routes `urllib.request.urlopen` through
`particle.http.fetch` instead of the underlying `http.client` /
sockets path (which the runtime can't satisfy).

Sibling to `_urllib3_compat` and `_httpx_compat`: same goal (idiomatic
HTTP libraries Just Work inside a particle), different patch surface
(monkey-patch `urllib.request.urlopen` rather than swapping classes
on an importable library).

Unlike the urllib3 / httpx hooks, this one fires unconditionally at
import time: `urllib.request` is in the frozen stdlib so it's always
present, and the patch is cheap. No meta_path watcher needed.
"""

import urllib.request as _urllib_request

from . import http as _http

__all__ = ["install_urllib_patch"]


def install_urllib_patch() -> None:
    """Replace `urllib.request.urlopen` with a wasi:http-routed
    equivalent. Idempotent — re-running is a no-op."""
    if getattr(_urllib_request, "_particle_patched", False):
        return

    _original_urlopen = _urllib_request.urlopen

    def _patched_urlopen(url, data=None, timeout=None, *args, **kwargs):
        # urllib.request.urlopen takes either a str URL or a
        # `Request` object that carries its own method / headers /
        # body. Mirror both forms before delegating to fetch.
        if isinstance(url, _urllib_request.Request):
            req = url
            target = req.full_url
            method = req.get_method()
            req_headers = {k: v for k, v in req.header_items()}
            req_body = req.data if data is None else data
        else:
            target = url
            method = "POST" if data is not None else "GET"
            req_headers = {}
            req_body = data

        resp = _http.fetch(target, method=method, headers=req_headers, body=req_body)
        return _UrllibResponseAdapter(resp, target)

    _urllib_request.urlopen = _patched_urlopen
    _urllib_request._particle_patched = True
    _urllib_request._particle_original_urlopen = _original_urlopen


class _UrllibResponseAdapter:
    """File-like adapter mimicking the `http.client.HTTPResponse`
    surface urllib callers expect.

    We expose the subset that's actually used in the wild — `.read()`,
    `.getcode()`, `.geturl()`, `.headers` (with `.get()`), context-
    manager protocol — which covers raw urllib calls and (historically)
    a fair chunk of HTTPS-y libraries built on top. urllib3 and httpx
    have their own hooks now; this one is for callers using urllib
    directly."""

    def __init__(self, resp: _http.Response, url: str):
        self._resp = resp
        self._url = url
        self._consumed = False

    def read(self, amt: int | None = None) -> bytes:
        if self._consumed:
            return b""
        self._consumed = True
        return self._resp.body if amt is None else self._resp.body[:amt]

    def getcode(self) -> int:
        return self._resp.status_code

    @property
    def status(self) -> int:
        return self._resp.status_code

    def geturl(self) -> str:
        return self._url

    @property
    def headers(self):
        return _UrllibHeaders(self._resp.headers)

    def info(self):
        return self.headers

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False

    def close(self):
        return None


class _UrllibHeaders:
    """`email.message.Message`-shaped header view — caller
    `.get("content-type", default)` keeps working."""

    def __init__(self, entries: list[tuple[str, bytes]]):
        self._entries = entries
        self._lookup = {k.lower(): v.decode("latin-1") for k, v in entries}

    def get(self, name: str, default=None):
        return self._lookup.get(name.lower(), default)

    def __getitem__(self, name: str):
        v = self._lookup.get(name.lower())
        if v is None:
            raise KeyError(name)
        return v

    def __contains__(self, name: str):
        return name.lower() in self._lookup

    def items(self):
        return [(k, v.decode("latin-1")) for k, v in self._entries]


# Install at module import. The bootstrap imports particle (which
# imports particle._urllib_compat) before loading the user bundle,
# so the patch is in place before any user `urllib.request.urlopen`
# runs.
install_urllib_patch()
