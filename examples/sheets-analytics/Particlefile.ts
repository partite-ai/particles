/**
 * Google Sheets analytics particle.
 *
 * Turns a spreadsheet into the agent's compute engine. Tools write
 * formulas (`=QUERY`, `=STDEV`, ...) and pivots/charts into the
 * sheet, then read evaluated results back — the user sees the
 * analysis materialize in their own spreadsheet.
 *
 *     particle build
 *     particle ping sheets-analytics
 *     particle run  sheets-analytics describe_sheet \
 *           --spreadsheet_id=<ID> --sheet=Sales
 *     particle run  sheets-analytics run_query \
 *           --spreadsheet_id=<ID> \
 *           --source_range='Sales!A:F' \
 *           --query='SELECT A, SUM(D) GROUP BY A ORDER BY SUM(D) DESC LIMIT 10'
 *     particle run  sheets-analytics add_pivot_table \
 *           --spreadsheet_id=<ID> \
 *           --source_range='Sales!A:F' \
 *           --row_columns=Region \
 *           --value_columns='[{"column":"Amount","function":"SUM"}]' \
 *           --target_sheet=Analytics
 *
 * Scratch space: `run_query` and `compute` need somewhere to land a
 * formula. We auto-create a hidden sheet called `__particle_scratch`
 * on first use; each call clears it before writing, so the most
 * recent computation is always inspectable in the spreadsheet.
 *
 * SDK: this particle is a demo of the @googleapis/sheets SDK working
 * inside a particle. credentials.getPlaceholder hands the SDK an
 * opaque token string; the host substitutes the real OAuth bearer
 * at the wasi:http boundary. The token never enters JS.
 */

import { sheets } from "npm:@googleapis/sheets@^9.0.0";
import { request as gaxiosRequest } from "npm:gaxios@^6.0.3";
import { credentials } from "@partite-ai/particle-credentials";

const SCRATCH_SHEET = "__particle_scratch";

// -----------------------------------------------------------------------------
// Build a configured Sheets client. Called once per tool invocation —
// the placeholder is fixed for the particle's lifetime so re-creating
// the client is fine.
//
// We must implement BOTH getRequestHeaders and request: googleapis-
// common@7.2.0 (apirequest.js:296-304) dispatches based on whether
// options.http2 is set — http2 calls getRequestHeaders and runs its
// own h2 transport, but the default non-http2 path calls
// authClient.request(options) directly and never consults
// getRequestHeaders. Sheets always takes the non-http2 path, so an
// auth shim with only getRequestHeaders silently sends unauthenticated
// requests. We delegate request() to gaxios (the same transport
// google-auth-library's DefaultTransporter uses) so the response shape
// matches what apirequest expects.
// -----------------------------------------------------------------------------

function client(): ReturnType<typeof sheets> {
  const info = credentials.getPlaceholder("google");
  const bearer = `Bearer ${info.placeholder}`;
  const auth = {
    getRequestHeaders: async () => ({ Authorization: bearer }),
    request: async (opts: Parameters<typeof gaxiosRequest>[0]) =>
      gaxiosRequest({
        ...opts,
        headers: { ...opts.headers, Authorization: bearer },
      }),
  };
  // The @googleapis/sheets `auth` parameter is typed against
  // google-auth-library's AuthClient, which has many more methods
  // than we use. Cast through unknown to satisfy the structural-
  // shaped surface without dragging the full SDK in.
  return sheets({ version: "v4", auth: auth as unknown as Parameters<typeof sheets>[0]["auth"] });
}

// -----------------------------------------------------------------------------
// Spreadsheet metadata (sheet IDs, dimensions, hidden flag) — read
// per tool call so concurrent invocations don't see each other's
// scratch state.
// -----------------------------------------------------------------------------

type SheetProps = {
  sheetId: number;
  title: string;
  hidden?: boolean;
  gridProperties: { rowCount: number; columnCount: number };
};

async function getMetadata(api: ReturnType<typeof sheets>, spreadsheetId: string) {
  const res = await api.spreadsheets.get({
    spreadsheetId,
    fields: "spreadsheetId,properties.title,sheets.properties(sheetId,title,hidden,gridProperties)",
  });
  return res.data;
}

function findSheet(meta: Awaited<ReturnType<typeof getMetadata>>, title: string): SheetProps | null {
  const s = meta.sheets?.find((s) => s.properties?.title === title);
  return s?.properties as SheetProps | undefined ?? null;
}

// -----------------------------------------------------------------------------
// A1 → GridRange parsing. Handles `Sheet1!A1:D10`, `'My Sheet'!A:D`,
// open bounds (which we fill in from sheet dimensions). 0-based
// indices; endRow / endColumn are exclusive (Sheets convention).
// -----------------------------------------------------------------------------

type GridRange = {
  sheetId: number;
  startRowIndex: number;
  endRowIndex: number;
  startColumnIndex: number;
  endColumnIndex: number;
};

function colLettersToIndex(letters: string): number {
  let n = 0;
  for (const ch of letters.toUpperCase()) {
    n = n * 26 + (ch.charCodeAt(0) - 64);
  }
  return n - 1;
}

function parseA1Range(a1: string): {
  sheetName: string | null;
  startRow: number | null;
  endRow:   number | null;
  startCol: number | null;
  endCol:   number | null;
} {
  let sheetName: string | null = null;
  let rest = a1;
  const bang = a1.lastIndexOf("!");
  if (bang >= 0) {
    let raw = a1.slice(0, bang);
    if (raw.startsWith("'") && raw.endsWith("'")) {
      raw = raw.slice(1, -1).replace(/''/g, "'");
    }
    sheetName = raw;
    rest = a1.slice(bang + 1);
  }
  if (rest === "") {
    return { sheetName, startRow: null, endRow: null, startCol: null, endCol: null };
  }
  const parseCell = (s: string) => {
    const m = s.match(/^([A-Z]+)?(\d+)?$/i);
    if (!m || (!m[1] && !m[2])) throw new Error(`bad A1 cell ${JSON.stringify(s)}`);
    return {
      col: m[1] ? colLettersToIndex(m[1]) : null,
      row: m[2] ? parseInt(m[2], 10) - 1 : null,
    };
  };
  const [a, b] = rest.split(":");
  const start = parseCell(a);
  const end = b ? parseCell(b) : start;
  return {
    sheetName,
    startRow: start.row,
    endRow:   end.row !== null ? end.row + 1 : null,
    startCol: start.col,
    endCol:   end.col !== null ? end.col + 1 : null,
  };
}

async function a1ToGridRange(
  api: ReturnType<typeof sheets>,
  spreadsheetId: string,
  a1: string,
  metaPromise?: ReturnType<typeof getMetadata>,
): Promise<GridRange> {
  const parsed = parseA1Range(a1);
  const meta = await (metaPromise ?? getMetadata(api, spreadsheetId));
  const sheet = parsed.sheetName
    ? findSheet(meta, parsed.sheetName)
    : (meta.sheets?.[0]?.properties as SheetProps | undefined);
  if (!sheet) throw new Error(`sheet ${JSON.stringify(parsed.sheetName)} not found`);
  const dims = sheet.gridProperties;
  return {
    sheetId: sheet.sheetId,
    startRowIndex:    parsed.startRow ?? 0,
    endRowIndex:      parsed.endRow   ?? dims.rowCount,
    startColumnIndex: parsed.startCol ?? 0,
    endColumnIndex:   parsed.endCol   ?? dims.columnCount,
  };
}

// -----------------------------------------------------------------------------
// Scratch sheet — created lazily, cleared each use so a debugging
// user sees only the most recent computation.
// -----------------------------------------------------------------------------

async function ensureScratchSheet(api: ReturnType<typeof sheets>, spreadsheetId: string): Promise<number> {
  const meta = await getMetadata(api, spreadsheetId);
  const existing = findSheet(meta, SCRATCH_SHEET);
  if (existing) return existing.sheetId;
  const res = await api.spreadsheets.batchUpdate({
    spreadsheetId,
    requestBody: {
      requests: [{
        addSheet: {
          properties: {
            title: SCRATCH_SHEET,
            hidden: true,
            gridProperties: { rowCount: 1000, columnCount: 26 },
          },
        },
      }],
    },
  });
  const sheetId = res.data.replies?.[0]?.addSheet?.properties?.sheetId;
  if (typeof sheetId !== "number") {
    throw new Error("scratch sheet creation returned no sheetId");
  }
  return sheetId;
}

async function clearScratch(api: ReturnType<typeof sheets>, spreadsheetId: string): Promise<void> {
  await api.spreadsheets.values.clear({
    spreadsheetId,
    range: `${SCRATCH_SHEET}!A1:Z1000`,
  });
}

async function writeScratchFormula(api: ReturnType<typeof sheets>, spreadsheetId: string, formula: string): Promise<void> {
  await api.spreadsheets.values.update({
    spreadsheetId,
    range: `${SCRATCH_SHEET}!A1`,
    valueInputOption: "USER_ENTERED",
    requestBody: { values: [[formula]] },
  });
}

async function readScratchRange(api: ReturnType<typeof sheets>, spreadsheetId: string, range: string): Promise<unknown[][]> {
  const res = await api.spreadsheets.values.get({
    spreadsheetId,
    range: `${SCRATCH_SHEET}!${range}`,
    valueRenderOption: "UNFORMATTED_VALUE",
  });
  return (res.data.values ?? []) as unknown[][];
}

// Trim trailing all-empty rows / cols so a spilled QUERY result
// returns just the populated rectangle, not the whole scratch range.
function trimEmpty(values: unknown[][]): unknown[][] {
  const isEmpty = (v: unknown) => v === undefined || v === null || v === "";
  let lastRow = -1, lastCol = -1;
  for (let r = 0; r < values.length; r++) {
    for (let c = 0; c < (values[r]?.length ?? 0); c++) {
      if (!isEmpty(values[r][c])) {
        if (r > lastRow) lastRow = r;
        if (c > lastCol) lastCol = c;
      }
    }
  }
  if (lastRow < 0) return [];
  return values.slice(0, lastRow + 1).map((row) => row.slice(0, lastCol + 1));
}

// -----------------------------------------------------------------------------
// Header-name → source-range-column-offset, for pivot tables / charts
// that let the agent reference columns by their header label.
// -----------------------------------------------------------------------------

async function getSourceHeaders(api: ReturnType<typeof sheets>, spreadsheetId: string, source_range: string): Promise<string[]> {
  const res = await api.spreadsheets.values.get({
    spreadsheetId,
    range: source_range,
    valueRenderOption: "UNFORMATTED_VALUE",
    majorDimension: "ROWS",
  });
  const first = (res.data.values?.[0] ?? []) as unknown[];
  return first.map((v) => String(v ?? ""));
}

function headerOffset(headers: string[], name: string): number {
  const i = headers.findIndex((h) => h === name);
  if (i < 0) {
    throw new Error(
      `column ${JSON.stringify(name)} not found; headers are: ${headers.join(", ")}`,
    );
  }
  return i;
}

async function resolveOrCreateTargetSheet(api: ReturnType<typeof sheets>, spreadsheetId: string, title: string): Promise<number> {
  const meta = await getMetadata(api, spreadsheetId);
  const existing = findSheet(meta, title);
  if (existing) return existing.sheetId;
  const res = await api.spreadsheets.batchUpdate({
    spreadsheetId,
    requestBody: { requests: [{ addSheet: { properties: { title } } }] },
  });
  const sheetId = res.data.replies?.[0]?.addSheet?.properties?.sheetId;
  if (typeof sheetId !== "number") {
    throw new Error(`target sheet creation for ${JSON.stringify(title)} returned no sheetId`);
  }
  return sheetId;
}

// -----------------------------------------------------------------------------
// Type inference for describe_sheet — hint the agent about each
// column without overstating certainty. Looks at the non-null
// sample values and picks the most-specific type that holds.
// -----------------------------------------------------------------------------

function inferType(samples: unknown[]): "number" | "boolean" | "date" | "string" | "empty" {
  const nonEmpty = samples.filter((v) => v !== undefined && v !== null && v !== "");
  if (nonEmpty.length === 0) return "empty";
  if (nonEmpty.every((v) => typeof v === "number")) return "number";
  if (nonEmpty.every((v) => typeof v === "boolean")) return "boolean";
  // UNFORMATTED_VALUE renders Sheets-native dates as serial numbers,
  // but ISO-string-typed cells stay strings. Heuristic on prefix.
  if (nonEmpty.every((v) => typeof v === "string" && /^\d{4}-\d{2}-\d{2}/.test(v))) return "date";
  return "string";
}

// -----------------------------------------------------------------------------
// Particle declaration
// -----------------------------------------------------------------------------

export default {
  name: "sheets-analytics",
  description: "Analyze Google Sheets data using the spreadsheet as the compute engine.",
  version: "0.1.0",

  capabilities: {
    // www.googleapis.com is added so ping can call Drive's
    // /about endpoint as a live "is the OAuth token actually
    // valid" probe — see the ping handler below.
    http: { allowedHosts: ["sheets.googleapis.com", "www.googleapis.com"] },
  },

  credentials: {
    google: {
      hosts: ["sheets.googleapis.com", "www.googleapis.com"],
      required: true,
      methods: {
        oauth: {
          type: "oauth2",
          description: "Sign in with Google",
          flows: ["authorization-code-pkce", "device-code"],
          scopes: [
            "https://www.googleapis.com/auth/spreadsheets",
            // drive.metadata.readonly is the narrowest scope
            // that authorizes /drive/v3/about; the ping
            // handler reads `user.emailAddress` from the
            // response to surface *which* account is signed
            // in, which is the most common "wrong account"
            // failure mode for this particle.
            "https://www.googleapis.com/auth/drive.metadata.readonly",
          ],
          authorizationUrl: "https://accounts.google.com/o/oauth2/v2/auth",
          tokenUrl:         "https://oauth2.googleapis.com/token",
          deviceAuthUrl:    "https://oauth2.googleapis.com/device/code",
        },
      },
    },
  },

  tools: {
    // -- inspection --------------------------------------------------

    describe_sheet: {
      description:
        "Inspect a sheet's shape before analyzing: returns header row, " +
        "row count, inferred column types, and a small sample. Call this " +
        "first to learn what columns you can reference in queries / pivots.",
      inputSchema: {
        type: "object",
        properties: {
          spreadsheet_id: { type: "string", description: "Spreadsheet ID from the URL." },
          sheet:          { type: "string", description: "Sheet / tab name." },
          sample_rows:    { type: "integer", description: "How many sample rows to return.", default: 3 },
        },
        required: ["spreadsheet_id", "sheet"],
      },
      handler: async (
        { spreadsheet_id, sheet, sample_rows = 3 }:
        { spreadsheet_id: string; sheet: string; sample_rows?: number },
      ) => {
        const api = client();
        const meta = await getMetadata(api, spreadsheet_id);
        const target = findSheet(meta, sheet);
        if (!target) {
          const available = (meta.sheets ?? []).map((s) => s.properties?.title).join(", ");
          throw new Error(`sheet ${JSON.stringify(sheet)} not found; available: ${available}`);
        }
        const res = await api.spreadsheets.values.get({
          spreadsheetId: spreadsheet_id,
          range: `${sheet}!A1:Z${1 + sample_rows}`,
          valueRenderOption: "UNFORMATTED_VALUE",
        });
        const rows = (res.data.values ?? []) as unknown[][];
        const headers = (rows[0] ?? []).map((v) => String(v ?? ""));
        const sample = rows.slice(1);
        const inferred_types = headers.map((_, c) => inferType(sample.map((row) => row[c])));
        return {
          spreadsheet_title: meta.properties?.title,
          sheet:        target.title,
          sheet_id:     target.sheetId,
          row_count:    target.gridProperties.rowCount,
          column_count: target.gridProperties.columnCount,
          headers,
          inferred_types,
          sample_rows: sample,
        };
      },
    },

    read_range: {
      description: "Read a range of cells as a 2-D array of unformatted values.",
      inputSchema: {
        type: "object",
        properties: {
          spreadsheet_id: { type: "string" },
          range:          { type: "string", description: "A1 range, e.g. \"Sales!A1:D100\" or \"Sales!A:D\"." },
        },
        required: ["spreadsheet_id", "range"],
      },
      handler: async (
        { spreadsheet_id, range }: { spreadsheet_id: string; range: string },
      ) => {
        const res = await client().spreadsheets.values.get({
          spreadsheetId: spreadsheet_id,
          range,
          valueRenderOption: "UNFORMATTED_VALUE",
        });
        return { range: res.data.range, values: res.data.values ?? [] };
      },
    },

    // -- compute: the sheet as a REPL --------------------------------

    run_query: {
      description:
        "Run a Google Sheets =QUERY against a source range. The query " +
        "language is a SQL-ish dialect: SELECT, WHERE, GROUP BY, ORDER " +
        "BY, PIVOT, LIMIT. Returns the result as a 2-D array (first row " +
        "is the result's headers when the query selects named columns).",
      inputSchema: {
        type: "object",
        properties: {
          spreadsheet_id: { type: "string" },
          source_range:   { type: "string", description: "A1 range that becomes the QUERY source, e.g. \"Sales!A:F\"." },
          query:          { type: "string", description: "Sheets QUERY string, e.g. \"SELECT A, SUM(D) GROUP BY A LIMIT 10\"." },
          num_headers:    { type: "integer", description: "Header rows in the source range (default 1).", default: 1 },
        },
        required: ["spreadsheet_id", "source_range", "query"],
      },
      handler: async (
        { spreadsheet_id, source_range, query, num_headers = 1 }:
        { spreadsheet_id: string; source_range: string; query: string; num_headers?: number },
      ) => {
        const api = client();
        await ensureScratchSheet(api, spreadsheet_id);
        await clearScratch(api, spreadsheet_id);
        const formula = `=QUERY(${source_range}, ${JSON.stringify(query)}, ${num_headers})`;
        await writeScratchFormula(api, spreadsheet_id, formula);
        const raw = await readScratchRange(api, spreadsheet_id, "A1:Z1000");
        if (raw.length === 1 && raw[0].length === 1 && typeof raw[0][0] === "string" && (raw[0][0] as string).startsWith("#")) {
          throw new Error(`QUERY error: ${raw[0][0]}`);
        }
        return { rows: trimEmpty(raw) };
      },
    },

    compute: {
      description:
        "Evaluate any Sheets formula and return the result. Use for " +
        "one-shot stats (=STDEV, =CORREL, =PERCENTILE, =FORECAST) and " +
        "lookups (=VLOOKUP, =GOOGLEFINANCE). The formula may reference " +
        "ranges in any sheet; it does NOT need to start with `=`.",
      inputSchema: {
        type: "object",
        properties: {
          spreadsheet_id: { type: "string" },
          formula:        { type: "string", description: "e.g. \"=STDEV(Sales!D2:D1000)\" or \"AVERAGE(B:B)\"." },
        },
        required: ["spreadsheet_id", "formula"],
      },
      handler: async (
        { spreadsheet_id, formula }: { spreadsheet_id: string; formula: string },
      ) => {
        const api = client();
        await ensureScratchSheet(api, spreadsheet_id);
        await clearScratch(api, spreadsheet_id);
        const normalized = formula.startsWith("=") ? formula : `=${formula}`;
        await writeScratchFormula(api, spreadsheet_id, normalized);
        const raw = await readScratchRange(api, spreadsheet_id, "A1:Z1000");
        if (raw.length === 0) return { value: null };
        if (raw.length === 1 && raw[0].length === 1) {
          const v = raw[0][0];
          if (typeof v === "string" && (v as string).startsWith("#")) {
            throw new Error(`formula error: ${v}`);
          }
          return { value: v };
        }
        // Spilled into multiple cells (e.g. =FILTER, =SORT, array UDFs).
        return { value: trimEmpty(raw) };
      },
    },

    // -- materialized analytics --------------------------------------

    add_pivot_table: {
      description:
        "Build a pivot table that summarizes a range and materialize " +
        "it into the target sheet (created if missing). Columns are " +
        "referenced by header name, looked up against the source " +
        "range's first row.",
      inputSchema: {
        type: "object",
        properties: {
          spreadsheet_id: { type: "string" },
          source_range:   { type: "string", description: "A1 range with a header row, e.g. \"Sales!A:F\"." },
          row_columns:    { type: "array", items: { type: "string" }, description: "Header names to group by on rows." },
          column_columns: { type: "array", items: { type: "string" }, description: "Header names to break out across columns (optional).", default: [] },
          value_columns: {
            type: "array",
            description: "Header names to aggregate, with the function to use.",
            items: {
              type: "object",
              properties: {
                column:   { type: "string" },
                function: { type: "string", enum: ["SUM", "AVERAGE", "COUNT", "COUNTA", "MIN", "MAX", "MEDIAN"], default: "SUM" },
              },
              required: ["column"],
            },
          },
          target_sheet: { type: "string", description: "Sheet to drop the pivot into.", default: "Analytics" },
        },
        required: ["spreadsheet_id", "source_range", "row_columns", "value_columns"],
      },
      handler: async (args: {
        spreadsheet_id: string;
        source_range:   string;
        row_columns:    string[];
        column_columns?: string[];
        value_columns:  Array<{ column: string; function?: string }>;
        target_sheet?:  string;
      }) => {
        const api = client();
        const target_sheet = args.target_sheet ?? "Analytics";
        const headers = await getSourceHeaders(api, args.spreadsheet_id, args.source_range);
        const sourceGrid = await a1ToGridRange(api, args.spreadsheet_id, args.source_range);
        const targetSheetId = await resolveOrCreateTargetSheet(api, args.spreadsheet_id, target_sheet);
        const offsetOf = (name: string) => headerOffset(headers, name);

        await api.spreadsheets.batchUpdate({
          spreadsheetId: args.spreadsheet_id,
          requestBody: {
            requests: [{
              updateCells: {
                rows: [{
                  values: [{
                    pivotTable: {
                      source: sourceGrid,
                      rows: args.row_columns.map((c) => ({
                        sourceColumnOffset: offsetOf(c),
                        showTotals: true,
                        sortOrder: "ASCENDING",
                      })),
                      columns: (args.column_columns ?? []).map((c) => ({
                        sourceColumnOffset: offsetOf(c),
                        showTotals: true,
                        sortOrder: "ASCENDING",
                      })),
                      values: args.value_columns.map((v) => ({
                        sourceColumnOffset: offsetOf(v.column),
                        summarizeFunction: v.function ?? "SUM",
                        name: `${v.function ?? "SUM"}(${v.column})`,
                      })),
                      valueLayout: "HORIZONTAL",
                    },
                  }],
                }],
                start: { sheetId: targetSheetId, rowIndex: 0, columnIndex: 0 },
                fields: "pivotTable",
              },
            }],
          },
        });
        return {
          target_sheet,
          target_sheet_id: targetSheetId,
          message: `pivot table written to ${target_sheet}!A1`,
        };
      },
    },

    add_chart: {
      description:
        "Embed a chart into the target sheet. First column of the " +
        "source range is the X axis / category labels; remaining " +
        "columns become series.",
      inputSchema: {
        type: "object",
        properties: {
          spreadsheet_id: { type: "string" },
          source_range:   { type: "string", description: "A1 range with header row, e.g. \"Sales!A1:C13\"." },
          chart_type:     { type: "string", enum: ["COLUMN", "BAR", "LINE", "AREA", "SCATTER", "PIE"], default: "COLUMN" },
          title:          { type: "string", default: "" },
          target_sheet:   { type: "string", description: "Sheet to drop the chart into.", default: "Analytics" },
        },
        required: ["spreadsheet_id", "source_range"],
      },
      handler: async (args: {
        spreadsheet_id: string;
        source_range:   string;
        chart_type?:    string;
        title?:         string;
        target_sheet?:  string;
      }) => {
        const api = client();
        const chart_type   = args.chart_type   ?? "COLUMN";
        const title        = args.title        ?? "";
        const target_sheet = args.target_sheet ?? "Analytics";

        const source = await a1ToGridRange(api, args.spreadsheet_id, args.source_range);
        const targetSheetId = await resolveOrCreateTargetSheet(api, args.spreadsheet_id, target_sheet);

        const numCols = source.endColumnIndex - source.startColumnIndex;
        if (numCols < 2) {
          throw new Error("source_range must include a label column and at least one value column");
        }
        // Schema$ChartData wraps a ChartSourceRange ({ sources: [...] }).
        const chartData = (startCol: number) => ({
          sourceRange: {
            sources: [{
              sheetId: source.sheetId,
              startRowIndex: source.startRowIndex,
              endRowIndex:   source.endRowIndex,
              startColumnIndex: startCol,
              endColumnIndex:   startCol + 1,
            }],
          },
        });
        const domain = chartData(source.startColumnIndex);
        const seriesData = [];
        for (let c = source.startColumnIndex + 1; c < source.endColumnIndex; c++) {
          seriesData.push(chartData(c));
        }

        // Pie charts use a different spec shape than basicChart.
        const spec = chart_type === "PIE"
          ? {
              title,
              pieChart: {
                legendPosition: "RIGHT_LEGEND",
                domain,
                series: seriesData[0],
              },
            }
          : {
              title,
              basicChart: {
                chartType: chart_type,
                legendPosition: "BOTTOM_LEGEND",
                headerCount: 1,
                domains: [{ domain }],
                series: seriesData.map((s) => ({ series: s, targetAxis: "LEFT_AXIS" })),
              },
            };

        const res = await api.spreadsheets.batchUpdate({
          spreadsheetId: args.spreadsheet_id,
          requestBody: {
            requests: [{
              addChart: {
                chart: {
                  spec,
                  position: {
                    overlayPosition: {
                      anchorCell: { sheetId: targetSheetId, rowIndex: 0, columnIndex: 0 },
                    },
                  },
                },
              },
            }],
          },
        });
        const chartId = res.data.replies?.[0]?.addChart?.chart?.chartId;
        return { chart_id: chartId, target_sheet, target_sheet_id: targetSheetId };
      },
    },
  },

  // Liveness probe — fires an unauthenticated-shaped request at
  // Drive's /about endpoint so a successful ping actually exercises
  // the OAuth round-trip end-to-end: the host substitutes the bearer
  // token, Google validates it, and we read back the signed-in
  // account. A 401/403 surfaces as unhealthy (token absent, expired,
  // or missing the drive.metadata.readonly scope); any other non-2xx
  // is degraded with the status text. We pick /about over a Sheets
  // call because Sheets has no scope-less liveness endpoint —
  // everything needs a real spreadsheet ID — and /about's response
  // ("which account am I?") is itself useful diagnostic output.
  ping: async () => {
    const fetcher = await credentials.fetcher("google");
    const url = "https://www.googleapis.com/drive/v3/about?fields=user(emailAddress,displayName)";
    let res: Response;
    try {
      res = await fetcher(url);
    } catch (err) {
      return {
        status: "unhealthy" as const,
        message: "request to Drive failed",
        details: String(err),
      };
    }
    if (res.status === 401 || res.status === 403) {
      const body = await res.text();
      return {
        status: "unhealthy" as const,
        message: `Google rejected the token (${res.status} ${res.statusText})`,
        details: body.length > 240 ? body.slice(0, 240) + "..." : body,
      };
    }
    if (!res.ok) {
      const body = await res.text();
      return {
        status: "degraded" as const,
        message: `Drive /about returned ${res.status} ${res.statusText}`,
        details: body.length > 240 ? body.slice(0, 240) + "..." : body,
      };
    }
    const body = await res.json() as {
      user?: { emailAddress?: string; displayName?: string };
    };
    const who = body.user?.emailAddress ?? body.user?.displayName ?? "(unknown account)";
    return {
      status: "ok" as const,
      message: `authenticated as ${who}`,
    };
  },
};
