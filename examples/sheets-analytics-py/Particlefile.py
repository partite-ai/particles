# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "google-api-python-client>=2.196.0",
#   "google-auth>=2.53.0",
# ]
# ///
"""
Google Sheets analytics particle (Python edition).

Turns a spreadsheet into the agent's compute engine. Tools write
formulas (`=QUERY`, `=STDEV`, ...) and pivots/charts into the sheet,
then read evaluated results back — the user sees the analysis
materialize in their own spreadsheet.

    particle build
    particle ping sheets-analytics-py
    particle run  sheets-analytics-py describe_sheet \\
          --spreadsheet_id=<ID> --sheet=Sales
    particle run  sheets-analytics-py run_query \\
          --spreadsheet_id=<ID> \\
          --source_range='Sales!A:F' \\
          --query='SELECT A, SUM(D) GROUP BY A ORDER BY SUM(D) DESC LIMIT 10'

Scratch space: `run_query` and `compute` need somewhere to land a
formula. We auto-create a hidden sheet called `__particle_scratch`
on first use; each call clears it before writing, so the most
recent computation is always inspectable in the spreadsheet.

SDK: this particle is a demo of the `google-api-python-client` SDK
working inside a particle. The SDK accepts a
`google.auth.credentials.Credentials` instance — we plug a small
subclass that surfaces `credentials.get_placeholder("google")` as a
bearer token. The host substitutes the real OAuth token at the
wasi:http boundary; the actual secret never enters Python.

httplib2 (the SDK's default transport) is auto-routed through
`particle.http.fetch` by `particle._httplib2_compat` — no per-call
plumbing needed.
"""

import json
import re

from googleapiclient.discovery import build
from google.auth import credentials as google_auth_credentials

from particle import credentials
from particle.manifest import Particle, Tool, Http, Credential, OAuth2


SCRATCH_SHEET = "__particle_scratch"


# -----------------------------------------------------------------------------
# Bridge between the particle credentials API and `google-auth`.
# `get_placeholder` returns an opaque string the host swaps for the
# real OAuth bearer when the wasi:http request leaves the sandbox; we
# wrap that placeholder in a Credentials subclass so the Sheets SDK
# treats it as a normal bearer token.
# -----------------------------------------------------------------------------

class _ParticlePlaceholderCredentials(google_auth_credentials.Credentials):
    """google-auth Credentials whose `token` is a particle placeholder.

    `refresh` is a no-op — the host owns the token lifecycle, so the
    placeholder is valid for the particle's lifetime. The base class
    treats `expiry is None` as "never expires", so `valid` stays
    True without us touching `expiry`.
    """

    def __init__(self, credential_name: str):
        super().__init__()
        self._credential_name = credential_name
        info = credentials.get_placeholder(credential_name)
        if info.apply.kind != credentials.ApplyKind.BEARER:
            raise RuntimeError(
                f"credential {credential_name!r} apply kind is {info.apply.kind!r}; "
                "Sheets needs an oauth2 (bearer) method"
            )
        self.token = info.placeholder

    def refresh(self, request):
        # Re-issue the placeholder. It doesn't actually change, but
        # google-auth calls this whenever it thinks the token might be
        # stale; doing the round-trip keeps the API honest.
        self.token = credentials.get_placeholder(self._credential_name).placeholder


# -----------------------------------------------------------------------------
# Sheets client — built per tool invocation. Cheap; the discovery doc
# is bundled in the google-api-python-client wheel so build() doesn't
# hit the network.
# -----------------------------------------------------------------------------

def _client():
    creds = _ParticlePlaceholderCredentials("google")
    # cache_discovery=False suppresses google-api-python-client's
    # attempt to write to ~/.cache — we have no writable filesystem
    # and the static bundled discovery doc is what we'd be loading
    # either way.
    return build("sheets", "v4", credentials=creds, cache_discovery=False)


# -----------------------------------------------------------------------------
# Spreadsheet metadata (sheet IDs, dimensions, hidden flag) — read
# per tool call so concurrent invocations don't see each other's
# scratch state.
# -----------------------------------------------------------------------------

def _get_metadata(api, spreadsheet_id):
    return api.spreadsheets().get(
        spreadsheetId=spreadsheet_id,
        fields="spreadsheetId,properties.title,sheets.properties(sheetId,title,hidden,gridProperties)",
    ).execute()


def _find_sheet(meta, title):
    for s in meta.get("sheets", []):
        props = s.get("properties") or {}
        if props.get("title") == title:
            return props
    return None


# -----------------------------------------------------------------------------
# A1 → GridRange parsing. Handles `Sheet1!A1:D10`, `'My Sheet'!A:D`,
# open bounds (filled in from sheet dimensions). 0-based indices;
# endRow / endColumn are exclusive (Sheets convention).
# -----------------------------------------------------------------------------

_CELL_RE = re.compile(r"^([A-Za-z]+)?(\d+)?$")


def _col_letters_to_index(letters):
    n = 0
    for ch in letters.upper():
        n = n * 26 + (ord(ch) - 64)
    return n - 1


def _parse_a1_range(a1):
    sheet_name = None
    rest = a1
    bang = a1.rfind("!")
    if bang >= 0:
        raw = a1[:bang]
        if raw.startswith("'") and raw.endswith("'"):
            raw = raw[1:-1].replace("''", "'")
        sheet_name = raw
        rest = a1[bang + 1 :]
    if rest == "":
        return {"sheet_name": sheet_name, "start_row": None, "end_row": None,
                "start_col": None, "end_col": None}

    def parse_cell(s):
        m = _CELL_RE.match(s)
        if not m or (not m.group(1) and not m.group(2)):
            raise ValueError(f"bad A1 cell {s!r}")
        col = _col_letters_to_index(m.group(1)) if m.group(1) else None
        row = int(m.group(2)) - 1 if m.group(2) else None
        return col, row

    parts = rest.split(":")
    a_col, a_row = parse_cell(parts[0])
    if len(parts) > 1:
        b_col, b_row = parse_cell(parts[1])
    else:
        b_col, b_row = a_col, a_row
    return {
        "sheet_name": sheet_name,
        "start_row": a_row,
        "end_row":   (b_row + 1) if b_row is not None else None,
        "start_col": a_col,
        "end_col":   (b_col + 1) if b_col is not None else None,
    }


def _a1_to_grid_range(api, spreadsheet_id, a1, meta=None):
    parsed = _parse_a1_range(a1)
    meta = meta or _get_metadata(api, spreadsheet_id)
    if parsed["sheet_name"]:
        sheet = _find_sheet(meta, parsed["sheet_name"])
    else:
        sheets_list = meta.get("sheets") or []
        sheet = (sheets_list[0].get("properties") if sheets_list else None)
    if not sheet:
        raise RuntimeError(f"sheet {parsed['sheet_name']!r} not found")
    dims = sheet.get("gridProperties") or {}
    return {
        "sheetId":          sheet["sheetId"],
        "startRowIndex":    parsed["start_row"] if parsed["start_row"] is not None else 0,
        "endRowIndex":      parsed["end_row"]   if parsed["end_row"]   is not None else dims.get("rowCount", 0),
        "startColumnIndex": parsed["start_col"] if parsed["start_col"] is not None else 0,
        "endColumnIndex":   parsed["end_col"]   if parsed["end_col"]   is not None else dims.get("columnCount", 0),
    }


# -----------------------------------------------------------------------------
# Scratch sheet — created lazily, cleared each use so a debugging
# user sees only the most recent computation.
# -----------------------------------------------------------------------------

def _ensure_scratch(api, spreadsheet_id):
    meta = _get_metadata(api, spreadsheet_id)
    existing = _find_sheet(meta, SCRATCH_SHEET)
    if existing:
        return existing["sheetId"]
    res = api.spreadsheets().batchUpdate(
        spreadsheetId=spreadsheet_id,
        body={
            "requests": [{
                "addSheet": {
                    "properties": {
                        "title": SCRATCH_SHEET,
                        "hidden": True,
                        "gridProperties": {"rowCount": 1000, "columnCount": 26},
                    },
                },
            }],
        },
    ).execute()
    sheet_id = (
        res.get("replies", [{}])[0]
           .get("addSheet", {})
           .get("properties", {})
           .get("sheetId")
    )
    if not isinstance(sheet_id, int):
        raise RuntimeError("scratch sheet creation returned no sheetId")
    return sheet_id


def _clear_scratch(api, spreadsheet_id):
    api.spreadsheets().values().clear(
        spreadsheetId=spreadsheet_id,
        range=f"{SCRATCH_SHEET}!A1:Z1000",
    ).execute()


def _write_scratch_formula(api, spreadsheet_id, formula):
    api.spreadsheets().values().update(
        spreadsheetId=spreadsheet_id,
        range=f"{SCRATCH_SHEET}!A1",
        valueInputOption="USER_ENTERED",
        body={"values": [[formula]]},
    ).execute()


def _read_scratch_range(api, spreadsheet_id, a1):
    res = api.spreadsheets().values().get(
        spreadsheetId=spreadsheet_id,
        range=f"{SCRATCH_SHEET}!{a1}",
        valueRenderOption="UNFORMATTED_VALUE",
    ).execute()
    return res.get("values", []) or []


def _trim_empty(values):
    """Trim trailing all-empty rows / cols so a spilled QUERY result
    returns just the populated rectangle, not the whole scratch range."""
    def empty(v):
        return v is None or v == ""
    last_row = -1
    last_col = -1
    for r, row in enumerate(values):
        for c, v in enumerate(row):
            if not empty(v):
                if r > last_row:
                    last_row = r
                if c > last_col:
                    last_col = c
    if last_row < 0:
        return []
    return [row[: last_col + 1] for row in values[: last_row + 1]]


# -----------------------------------------------------------------------------
# Header-name → source-range-column-offset, for pivot tables / charts
# that let the agent reference columns by their header label.
# -----------------------------------------------------------------------------

def _get_source_headers(api, spreadsheet_id, source_range):
    res = api.spreadsheets().values().get(
        spreadsheetId=spreadsheet_id,
        range=source_range,
        valueRenderOption="UNFORMATTED_VALUE",
        majorDimension="ROWS",
    ).execute()
    first = (res.get("values") or [[]])[0]
    return ["" if v is None else str(v) for v in first]


def _header_offset(headers, name):
    try:
        return headers.index(name)
    except ValueError:
        raise RuntimeError(
            f"column {name!r} not found; headers are: {', '.join(headers)}"
        )


def _resolve_or_create_target_sheet(api, spreadsheet_id, title):
    meta = _get_metadata(api, spreadsheet_id)
    existing = _find_sheet(meta, title)
    if existing:
        return existing["sheetId"]
    res = api.spreadsheets().batchUpdate(
        spreadsheetId=spreadsheet_id,
        body={"requests": [{"addSheet": {"properties": {"title": title}}}]},
    ).execute()
    sheet_id = (
        res.get("replies", [{}])[0]
           .get("addSheet", {})
           .get("properties", {})
           .get("sheetId")
    )
    if not isinstance(sheet_id, int):
        raise RuntimeError(f"target sheet creation for {title!r} returned no sheetId")
    return sheet_id


# -----------------------------------------------------------------------------
# Type inference for describe_sheet — hint the agent about each
# column without overstating certainty.
# -----------------------------------------------------------------------------

_ISO_DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}")


def _infer_type(samples):
    non_empty = [v for v in samples if v is not None and v != ""]
    if not non_empty:
        return "empty"
    # bool is a subclass of int in Python; check bool first so a
    # column of True/False isn't reported as "number".
    if all(isinstance(v, bool) for v in non_empty):
        return "boolean"
    if all(isinstance(v, (int, float)) and not isinstance(v, bool) for v in non_empty):
        return "number"
    # UNFORMATTED_VALUE renders Sheets-native dates as serial numbers,
    # but ISO-string-typed cells stay strings. Heuristic on prefix.
    if all(isinstance(v, str) and _ISO_DATE_RE.match(v) for v in non_empty):
        return "date"
    return "string"


# -----------------------------------------------------------------------------
# Tool handlers
# -----------------------------------------------------------------------------

def _describe_sheet(args):
    spreadsheet_id = args["spreadsheet_id"]
    sheet_name = args["sheet"]
    sample_rows = int(args.get("sample_rows", 3))

    api = _client()
    meta = _get_metadata(api, spreadsheet_id)
    target = _find_sheet(meta, sheet_name)
    if not target:
        available = ", ".join(
            (s.get("properties") or {}).get("title", "") for s in meta.get("sheets", [])
        )
        raise RuntimeError(f"sheet {sheet_name!r} not found; available: {available}")
    res = api.spreadsheets().values().get(
        spreadsheetId=spreadsheet_id,
        range=f"{sheet_name}!A1:Z{1 + sample_rows}",
        valueRenderOption="UNFORMATTED_VALUE",
    ).execute()
    rows = res.get("values", []) or []
    headers = ["" if v is None else str(v) for v in (rows[0] if rows else [])]
    sample = rows[1:]
    inferred_types = [
        _infer_type([(row[c] if c < len(row) else None) for row in sample])
        for c in range(len(headers))
    ]
    return {
        "spreadsheet_title": (meta.get("properties") or {}).get("title"),
        "sheet":        target["title"],
        "sheet_id":     target["sheetId"],
        "row_count":    (target.get("gridProperties") or {}).get("rowCount"),
        "column_count": (target.get("gridProperties") or {}).get("columnCount"),
        "headers":      headers,
        "inferred_types": inferred_types,
        "sample_rows":  sample,
    }


def _read_range(args):
    res = _client().spreadsheets().values().get(
        spreadsheetId=args["spreadsheet_id"],
        range=args["range"],
        valueRenderOption="UNFORMATTED_VALUE",
    ).execute()
    return {"range": res.get("range"), "values": res.get("values", [])}


def _run_query(args):
    spreadsheet_id = args["spreadsheet_id"]
    source_range = args["source_range"]
    query = args["query"]
    num_headers = int(args.get("num_headers", 1))

    api = _client()
    _ensure_scratch(api, spreadsheet_id)
    _clear_scratch(api, spreadsheet_id)
    formula = f"=QUERY({source_range}, {json.dumps(query)}, {num_headers})"
    _write_scratch_formula(api, spreadsheet_id, formula)
    raw = _read_scratch_range(api, spreadsheet_id, "A1:Z1000")
    if (
        len(raw) == 1
        and len(raw[0]) == 1
        and isinstance(raw[0][0], str)
        and raw[0][0].startswith("#")
    ):
        raise RuntimeError(f"QUERY error: {raw[0][0]}")
    return {"rows": _trim_empty(raw)}


def _compute(args):
    spreadsheet_id = args["spreadsheet_id"]
    formula = args["formula"]

    api = _client()
    _ensure_scratch(api, spreadsheet_id)
    _clear_scratch(api, spreadsheet_id)
    normalized = formula if formula.startswith("=") else f"={formula}"
    _write_scratch_formula(api, spreadsheet_id, normalized)
    raw = _read_scratch_range(api, spreadsheet_id, "A1:Z1000")
    if not raw:
        return {"value": None}
    if len(raw) == 1 and len(raw[0]) == 1:
        v = raw[0][0]
        if isinstance(v, str) and v.startswith("#"):
            raise RuntimeError(f"formula error: {v}")
        return {"value": v}
    # Spilled into multiple cells (e.g. =FILTER, =SORT, array UDFs).
    return {"value": _trim_empty(raw)}


def _add_pivot_table(args):
    api = _client()
    spreadsheet_id = args["spreadsheet_id"]
    source_range = args["source_range"]
    row_columns = args["row_columns"]
    column_columns = args.get("column_columns") or []
    value_columns = args["value_columns"]
    target_sheet = args.get("target_sheet") or "Analytics"

    headers = _get_source_headers(api, spreadsheet_id, source_range)
    source_grid = _a1_to_grid_range(api, spreadsheet_id, source_range)
    target_sheet_id = _resolve_or_create_target_sheet(api, spreadsheet_id, target_sheet)

    def offset(name):
        return _header_offset(headers, name)

    pivot = {
        "source": source_grid,
        "rows": [
            {"sourceColumnOffset": offset(c), "showTotals": True, "sortOrder": "ASCENDING"}
            for c in row_columns
        ],
        "columns": [
            {"sourceColumnOffset": offset(c), "showTotals": True, "sortOrder": "ASCENDING"}
            for c in column_columns
        ],
        "values": [
            {
                "sourceColumnOffset": offset(v["column"]),
                "summarizeFunction": v.get("function", "SUM"),
                "name": f"{v.get('function', 'SUM')}({v['column']})",
            }
            for v in value_columns
        ],
        "valueLayout": "HORIZONTAL",
    }

    api.spreadsheets().batchUpdate(
        spreadsheetId=spreadsheet_id,
        body={
            "requests": [{
                "updateCells": {
                    "rows": [{"values": [{"pivotTable": pivot}]}],
                    "start": {"sheetId": target_sheet_id, "rowIndex": 0, "columnIndex": 0},
                    "fields": "pivotTable",
                },
            }],
        },
    ).execute()
    return {
        "target_sheet":    target_sheet,
        "target_sheet_id": target_sheet_id,
        "message": f"pivot table written to {target_sheet}!A1",
    }


def _add_chart(args):
    api = _client()
    spreadsheet_id = args["spreadsheet_id"]
    source_range = args["source_range"]
    chart_type = args.get("chart_type") or "COLUMN"
    title = args.get("title") or ""
    target_sheet = args.get("target_sheet") or "Analytics"

    source = _a1_to_grid_range(api, spreadsheet_id, source_range)
    target_sheet_id = _resolve_or_create_target_sheet(api, spreadsheet_id, target_sheet)

    num_cols = source["endColumnIndex"] - source["startColumnIndex"]
    if num_cols < 2:
        raise RuntimeError(
            "source_range must include a label column and at least one value column"
        )

    def chart_data(start_col):
        return {
            "sourceRange": {
                "sources": [{
                    "sheetId":          source["sheetId"],
                    "startRowIndex":    source["startRowIndex"],
                    "endRowIndex":      source["endRowIndex"],
                    "startColumnIndex": start_col,
                    "endColumnIndex":   start_col + 1,
                }],
            },
        }

    domain = chart_data(source["startColumnIndex"])
    series_data = [
        chart_data(c)
        for c in range(source["startColumnIndex"] + 1, source["endColumnIndex"])
    ]

    if chart_type == "PIE":
        spec = {
            "title": title,
            "pieChart": {
                "legendPosition": "RIGHT_LEGEND",
                "domain": domain,
                "series": series_data[0],
            },
        }
    else:
        spec = {
            "title": title,
            "basicChart": {
                "chartType":      chart_type,
                "legendPosition": "BOTTOM_LEGEND",
                "headerCount":    1,
                "domains": [{"domain": domain}],
                "series": [{"series": s, "targetAxis": "LEFT_AXIS"} for s in series_data],
            },
        }

    res = api.spreadsheets().batchUpdate(
        spreadsheetId=spreadsheet_id,
        body={
            "requests": [{
                "addChart": {
                    "chart": {
                        "spec": spec,
                        "position": {
                            "overlayPosition": {
                                "anchorCell": {
                                    "sheetId":     target_sheet_id,
                                    "rowIndex":    0,
                                    "columnIndex": 0,
                                },
                            },
                        },
                    },
                },
            }],
        },
    ).execute()
    chart_id = (
        res.get("replies", [{}])[0]
           .get("addChart", {})
           .get("chart", {})
           .get("chartId")
    )
    return {
        "chart_id":        chart_id,
        "target_sheet":    target_sheet,
        "target_sheet_id": target_sheet_id,
    }


def _ping():
    """Cheap liveness probe — confirms a credential is configured. We
    don't fire a Sheets call here because a healthy ping shouldn't
    need to know any specific spreadsheet ID; the configured-method
    check covers the common failure mode (user ran build but never
    finished OAuth setup).
    """
    method = credentials.get_configured_method("google")
    if method is None:
        return {"status": "unhealthy", "message": "no Google credential configured"}
    return {"status": "ok", "message": f"Google credential configured via {method}"}


# -----------------------------------------------------------------------------
# Particle declaration
# -----------------------------------------------------------------------------

particle = Particle(
    name="sheets-analytics-py",
    description="Analyze Google Sheets data using the spreadsheet as the compute engine. (Python edition.)",
    version="0.1.0",

    http=Http(allowed_hosts=["sheets.googleapis.com"]),

    credentials={
        "google": Credential(
            hosts=["sheets.googleapis.com"],
            required=True,
            methods={
                "oauth": OAuth2(
                    description="Sign in with Google",
                    flows=["authorization-code-pkce", "device-code"],
                    scopes=["https://www.googleapis.com/auth/spreadsheets"],
                    authorization_url="https://accounts.google.com/o/oauth2/v2/auth",
                    token_url="https://oauth2.googleapis.com/token",
                    device_auth_url="https://oauth2.googleapis.com/device/code",
                ),
            },
        ),
    },

    tools={
        "describe_sheet": Tool(
            description=(
                "Inspect a sheet's shape before analyzing: returns header row, "
                "row count, inferred column types, and a small sample. Call this "
                "first to learn what columns you can reference in queries / pivots."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "spreadsheet_id": {"type": "string", "description": "Spreadsheet ID from the URL."},
                    "sheet":          {"type": "string", "description": "Sheet / tab name."},
                    "sample_rows":    {"type": "integer", "description": "How many sample rows to return.", "default": 3},
                },
                "required": ["spreadsheet_id", "sheet"],
            },
            handler=_describe_sheet,
        ),

        "read_range": Tool(
            description="Read a range of cells as a 2-D array of unformatted values.",
            input_schema={
                "type": "object",
                "properties": {
                    "spreadsheet_id": {"type": "string"},
                    "range":          {"type": "string", "description": "A1 range, e.g. \"Sales!A1:D100\" or \"Sales!A:D\"."},
                },
                "required": ["spreadsheet_id", "range"],
            },
            handler=_read_range,
        ),

        "run_query": Tool(
            description=(
                "Run a Google Sheets =QUERY against a source range. The query "
                "language is a SQL-ish dialect: SELECT, WHERE, GROUP BY, ORDER "
                "BY, PIVOT, LIMIT. Returns the result as a 2-D array (first row "
                "is the result's headers when the query selects named columns)."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "spreadsheet_id": {"type": "string"},
                    "source_range":   {"type": "string", "description": "A1 range that becomes the QUERY source, e.g. \"Sales!A:F\"."},
                    "query":          {"type": "string", "description": "Sheets QUERY string, e.g. \"SELECT A, SUM(D) GROUP BY A LIMIT 10\"."},
                    "num_headers":    {"type": "integer", "description": "Header rows in the source range (default 1).", "default": 1},
                },
                "required": ["spreadsheet_id", "source_range", "query"],
            },
            handler=_run_query,
        ),

        "compute": Tool(
            description=(
                "Evaluate any Sheets formula and return the result. Use for "
                "one-shot stats (=STDEV, =CORREL, =PERCENTILE, =FORECAST) and "
                "lookups (=VLOOKUP, =GOOGLEFINANCE). The formula may reference "
                "ranges in any sheet; it does NOT need to start with `=`."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "spreadsheet_id": {"type": "string"},
                    "formula":        {"type": "string", "description": "e.g. \"=STDEV(Sales!D2:D1000)\" or \"AVERAGE(B:B)\"."},
                },
                "required": ["spreadsheet_id", "formula"],
            },
            handler=_compute,
        ),

        "add_pivot_table": Tool(
            description=(
                "Build a pivot table that summarizes a range and materialize "
                "it into the target sheet (created if missing). Columns are "
                "referenced by header name, looked up against the source "
                "range's first row."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "spreadsheet_id": {"type": "string"},
                    "source_range":   {"type": "string", "description": "A1 range with a header row, e.g. \"Sales!A:F\"."},
                    "row_columns":    {"type": "array", "items": {"type": "string"}, "description": "Header names to group by on rows."},
                    "column_columns": {"type": "array", "items": {"type": "string"}, "description": "Header names to break out across columns (optional).", "default": []},
                    "value_columns": {
                        "type": "array",
                        "description": "Header names to aggregate, with the function to use.",
                        "items": {
                            "type": "object",
                            "properties": {
                                "column":   {"type": "string"},
                                "function": {"type": "string", "enum": ["SUM", "AVERAGE", "COUNT", "COUNTA", "MIN", "MAX", "MEDIAN"], "default": "SUM"},
                            },
                            "required": ["column"],
                        },
                    },
                    "target_sheet": {"type": "string", "description": "Sheet to drop the pivot into.", "default": "Analytics"},
                },
                "required": ["spreadsheet_id", "source_range", "row_columns", "value_columns"],
            },
            handler=_add_pivot_table,
        ),

        "add_chart": Tool(
            description=(
                "Embed a chart into the target sheet. First column of the "
                "source range is the X axis / category labels; remaining "
                "columns become series."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "spreadsheet_id": {"type": "string"},
                    "source_range":   {"type": "string", "description": "A1 range with header row, e.g. \"Sales!A1:C13\"."},
                    "chart_type":     {"type": "string", "enum": ["COLUMN", "BAR", "LINE", "AREA", "SCATTER", "PIE"], "default": "COLUMN"},
                    "title":          {"type": "string", "default": ""},
                    "target_sheet":   {"type": "string", "description": "Sheet to drop the chart into.", "default": "Analytics"},
                },
                "required": ["spreadsheet_id", "source_range"],
            },
            handler=_add_chart,
        ),
    },

    ping=_ping,
)
