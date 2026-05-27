# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""Smoke test for the async wasi:http integration.

Drives `particle.http.async_fetch` directly (no aiohttp wheel needed
for this test — aiohttp packaging is its own problem; the underlying
runtime mechanism is what we validate here).
"""

import asyncio
import time

from particle import http as particle_http
from particle.manifest import Particle, Tool, Http


URL = "https://api.github.com/zen"


def _gather(args):
    """Fire N requests concurrently and report whether they overlapped.

    Each request takes roughly the same time host-side; if our async
    loop overlaps them, total wall time should be a fraction of
    n * single_request_time, not the full sum. The ratio is reported
    so callers can eyeball the speedup.
    """
    n = int(args.get("n", 3))

    async def fetch_one(i):
        resp = await particle_http.async_fetch(URL)
        return {"i": i, "status": resp.status_code, "body_bytes": len(resp.body)}

    async def main():
        t0 = time.monotonic()
        results = await asyncio.gather(*(fetch_one(i) for i in range(n)))
        elapsed = time.monotonic() - t0
        return {
            "elapsed_seconds": round(elapsed, 3),
            "requests": n,
            "results": results,
        }

    return asyncio.run(main())


def _sequential(args):
    """Same N requests, but awaited one-at-a-time. Sanity baseline.
    Compare elapsed_seconds against the gather variant."""
    n = int(args.get("n", 3))

    async def main():
        t0 = time.monotonic()
        results = []
        for i in range(n):
            resp = await particle_http.async_fetch(URL)
            results.append({"i": i, "status": resp.status_code, "body_bytes": len(resp.body)})
        elapsed = time.monotonic() - t0
        return {
            "elapsed_seconds": round(elapsed, 3),
            "requests": n,
            "results": results,
        }

    return asyncio.run(main())


def _one(args):
    """Single request — exercises the basic async path."""
    async def main():
        resp = await particle_http.async_fetch(URL)
        return {"status": resp.status_code, "body_len": len(resp.body)}

    return asyncio.run(main())


particle = Particle(
    name="aiohttp-test",
    description="Smoke test for async wasi:http.",
    version="0.1.0",
    http=Http(allowed_hosts=["api.github.com"]),
    tools={
        "one": Tool(
            description="Fetch a single URL.",
            input_schema={"type": "object", "properties": {}},
            handler=_one,
        ),
        "gather": Tool(
            description="Fire N concurrent requests via asyncio.gather; check they overlap.",
            input_schema={
                "type": "object",
                "properties": {"n": {"type": "integer", "default": 3}},
            },
            handler=_gather,
        ),
        "sequential": Tool(
            description="Same as gather but await sequentially — baseline.",
            input_schema={
                "type": "object",
                "properties": {"n": {"type": "integer", "default": 3}},
            },
            handler=_sequential,
        ),
    },
)
