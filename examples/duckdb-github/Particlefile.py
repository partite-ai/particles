# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "duckdb",
# ]
# ///
"""
Discover GitHub repositories with DuckDB, write the results as Markdown.

This is the particle port of the DuckDB blog post "Discovering DuckDB
Use Cases via GitHub" (https://duckdb.org/2025/06/27/discovering-w-github)
— it queries the GitHub search API, shapes the JSON into a Markdown
table entirely in SQL, and writes the file to a mounted directory.

Two capabilities, matching the blog's two needs:

  * an `apikey` credential — a GitHub PAT, sent as `Authorization:
    Bearer <token>`; and
  * one read-write filesystem mount — where the `.md` report lands.

The interesting part is *how the token reaches DuckDB*. The blog does:

    CREATE SECRET http_auth (TYPE http, BEARER_TOKEN '<real token>', ...);

In a particle the real token never enters the sandbox. Instead the host
hands us an opaque *placeholder* (`credentials.get_placeholder`), and we
plant that placeholder as DuckDB's `BEARER_TOKEN`. DuckDB then emits
`Authorization: Bearer <placeholder>` on its httpfs request; because the
runtime routes DuckDB's HTTP through the particle HTTP layer, the host
recognizes the placeholder at the wasi:http boundary and swaps in the
real PAT as the request leaves wasm. A `BEARER_TOKEN` placed anywhere the
apply-spec doesn't expect would transmit literally and GitHub would 401.

    particle build
    particle mount duckdb-github out ./reports
    particle ping  duckdb-github
    particle run   duckdb-github discover_repos --query=duckdb --filename=duckdb.md
    particle run   duckdb-github discover_repos \\
          --query=duckdb --days=7 --max_pages=3 --exclude_owner=duckdb
"""

import math
import urllib.parse
from datetime import datetime, timedelta, timezone

import duckdb

from particle import credentials
from particle.manifest import (
    Particle, Tool, Http, Filesystem, Mount,
    Credential, ApiKey, ApiKeyLocation, TempMount
)

SEARCH_API = "https://api.github.com/search/repositories"
SEARCH_SCOPE = "https://api.github.com/search"
OUT_DIR = "/mnt/out"

# GitHub's search API caps results at 1000 (10 pages of 100) and
# returns 100 results per page at most.
PER_PAGE = 100
MAX_RESULTS = 1000


def _create_github_secret(conn):
    """Plant the `github` credential placeholder as DuckDB's bearer token.

    The host substitutes the real PAT only when it sees the placeholder
    in exactly `Authorization: Bearer <placeholder>` — which is what
    DuckDB's `TYPE http` secret with `BEARER_TOKEN` emits. `SCOPE` pins
    the secret to the search API so it isn't attached to unrelated URLs.
    """
    token = credentials.get_placeholder("github")
    conn.execute(
        f"CREATE OR REPLACE SECRET github_http "
        f"(TYPE http, BEARER_TOKEN '{token.placeholder}', SCOPE '{SEARCH_SCOPE}');"
    )


def _page_url(query: str, cutoff: str, page: int) -> str:
    """Build a search-API URL for one page. `cutoff` (YYYY-MM-DD), when
    set, restricts to repos pushed on or after that date — the blog's
    trick to stay under the 1000-result cap on busy queries."""
    q = query if not cutoff else f"{query} pushed:>={cutoff}"
    params = urllib.parse.urlencode(
        {"q": q, "per_page": PER_PAGE, "page": page, "sort": "updated"}
    )
    return f"{SEARCH_API}?{params}"


def _discover(args):
    query = args.get("query", "duckdb")
    filename = args.get("filename", "repositories.md")
    days = int(args.get("days", 7))
    max_pages = int(args.get("max_pages", 3))
    min_activity = int(args.get("min_activity", 3))
    exclude_owner = args.get("exclude_owner")  # optional

    # The file is written into the mount root; reject anything that could
    # escape it. (The sandbox would deny a traversal anyway, but a clear
    # error beats an OSError.)
    if "/" in filename or "\\" in filename or filename in ("", ".", ".."):
        raise ValueError("filename must be a plain file name, no path separators")

    cutoff = ""
    if days > 0:
        cutoff = (datetime.now(timezone.utc) - timedelta(days=days)).strftime("%Y-%m-%d")

    conn = duckdb.connect()
    _create_github_secret(conn)
    conn.execute("SET force_download = true;")
    conn.execute("SET temp_directory = '/tmp/duckdb_swap';")
    conn.execute("SET memory_limit = '100MB';")

    # Phase 1 — one request just to learn the match count, so we know how
    # many pages to ask for. total_count is repeated on every page.
    first = conn.read_json(_page_url(query, cutoff, 1))
    total = first.aggregate("max(total_count) AS t").fetchone()[0] or 0
    reachable = min(int(total), MAX_RESULTS)
    pages = max(1, math.ceil(reachable / PER_PAGE))
    pages = min(pages, max_pages)

    # Phase 2 — read every page in a single read_json so DuckDB unifies
    # the (deeply nested) schema across pages. Page 1 is fetched a second
    # time here; the alternative — appending each page into a table whose
    # schema was fixed by page 1 — breaks when a later page infers a
    # slightly different shape (e.g. an all-null field).
    urls = [_page_url(query, cutoff, p) for p in range(1, pages + 1)]
    conn.read_json(urls, union_by_name=True).to_table("github_raw_data")

    # Transform to one Markdown table row per repo. `unnest(items)` yields
    # the per-repo struct `r`; we read fields explicitly (rather than the
    # blog's recursive unnest, which relies on positional `name_1`-style
    # column dedup). A repo is kept if it isn't a fork and its
    # stars+open_issues+forks activity clears the threshold.
    where = (
        "NOT r.fork AND "
        "(r.stargazers_count + r.open_issues_count + r.forks_count) >= $min_activity"
    )
    params = {"min_activity": min_activity}
    if exclude_owner:
        where += " AND lower(r.owner.login) <> lower($exclude_owner)"
        params["exclude_owner"] = exclude_owner

    sql = f"""
        WITH flat AS (
            SELECT unnest(items) AS r FROM github_raw_data
        )
        SELECT concat('|', concat_ws('|',
            concat_ws('<br>',
                concat('[', r.name, '](https://github.com/', r.full_name, ')'),
                -- strip newlines and pipes so the cell can't break the table
                replace(replace(replace(coalesce(r.description, ' '),
                    chr(10), ' '), chr(13), ' '), '|', ' '),
                concat('**License** ', coalesce(r.license.name, 'unknown')),
                concat('**Owner** ', r.owner.login)),
            coalesce(array_to_string(r.topics, ', '), ''),
            r.stargazers_count::VARCHAR,
            r.open_issues_count::VARCHAR,
            r.forks_count::VARCHAR,
            r.created_at::VARCHAR,
            r.updated_at::VARCHAR
        ), '|') AS line
        FROM flat AS f
        WHERE {where}
        ORDER BY (r.stargazers_count + r.open_issues_count + r.forks_count) DESC
    """
    lines = [row[0] for row in conn.execute(sql, params).fetchall()]

    header = "|Name|Topics|Stars|Open Issues|Forks|Created At|Updated At|"
    delimiter = "|" + "|".join(["--"] * 7) + "|"
    doc = "\n".join(
        [f"# Repositories matching `{query}`", "", header, delimiter, *lines]
    ) + "\n"

    out_path = f"{OUT_DIR}/{filename}"
    with open(out_path, "w") as fh:
        fh.write(doc)

    return {
        "file": out_path,
        "repos_written": len(lines),
        "total_matches": int(total),
        "pages_fetched": pages,
        "query": query,
    }


def _ping():
    """Confirm the DuckDB wheel loads in-sandbox and report whether a
    GitHub credential is configured. Makes no network call."""
    try:
        version = duckdb.connect().sql("SELECT version()").fetchone()[0]
    except Exception as exc:  # noqa: BLE001 — surface any load failure
        return {"status": "unhealthy", "message": f"DuckDB failed to load: {exc}"}
    method = credentials.get_configured_method("github")
    if method is None:
        return {
            "status": "degraded",
            "message": f"DuckDB {version} loaded, but no github credential configured",
        }
    return {"status": "ok", "message": f"DuckDB {version}; github via {method}"}


particle = Particle(
    name="duckdb-github",
    description="Query GitHub for repositories with DuckDB and write a Markdown report.",
    version="0.1.0",

    http=Http(allowed_hosts=["api.github.com"]),

    credentials={
        "github": Credential(
            # Substitution only fires on api.github.com; the placeholder
            # planted as DuckDB's BEARER_TOKEN is swapped for the real PAT
            # as the search request leaves the sandbox.
            hosts=["api.github.com"],
            required=True,
            methods={
                "pat": ApiKey(
                    description="GitHub personal access token (read access to public repos).",
                    location=ApiKeyLocation(kind="auth-scheme", scheme="Bearer"),
                ),
            },
        ),
    },

    filesystem=Filesystem(
        mounts={
            "out": Mount(
                description="Directory the Markdown report is written into.",
                path=OUT_DIR,
                access="readwrite",
                required=True,
            ),
        },
        temp={
            "duckdb": TempMount(
                description="DuckDB's scratch space for sorting and joins that exceed memory; cleared on exit.",
                path="/tmp/duckdb_swap",
                max_size="100MB",
            )
        },
    ),

    tools={
        "discover_repos": Tool(
            description=(
                "Search GitHub for repositories matching a term and write a "
                "Markdown table (name, topics, stars, issues, forks, dates) to "
                "the mounted output directory."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "query": {
                        "type": "string",
                        "description": "Search term, e.g. 'duckdb'.",
                        "default": "duckdb",
                    },
                    "filename": {
                        "type": "string",
                        "description": "Output file name in the mount (no path separators).",
                        "default": "repositories.md",
                    },
                    "days": {
                        "type": "integer",
                        "description": "Only repos pushed within the last N days; 0 to disable.",
                        "default": 7,
                    },
                    "max_pages": {
                        "type": "integer",
                        "description": "Cap on pages fetched (100 repos per page).",
                        "default": 3,
                    },
                    "min_activity": {
                        "type": "integer",
                        "description": "Keep repos whose stars+open_issues+forks is at least this.",
                        "default": 3,
                    },
                    "exclude_owner": {
                        "type": "string",
                        "description": "Optional owner login to filter out (e.g. the project's own org).",
                    },
                },
            },
            handler=_discover,
        ),
    },

    ping=_ping,
)
