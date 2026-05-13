/**
 * Example particle exposing a few GitHub REST endpoints as tools.
 *
 * Authentication: two alternatives — OAuth 2.0 or a personal access
 * token (PAT). At setup the user picks one. Tools call
 * `credentials.getConfiguredMethod()` to discover which method
 * was chosen, then route through `credentials.fetcher(name)`.
 *
 *   particle build           # walks `particle setup` interactively
 *   particle ping github-tools
 *   particle run  github-tools list_issues --owner=octocat --repo=hello-world
 *   particle run  github-tools create_issue --owner=me --repo=mine --title="Bug"
 */

import { credentials } from "@partite-ai/particle-credentials";

// Resolve the active credential's name. The host call is sync and
// the result is fixed at setup time, so we just read it directly
// where we need it.
function authMethod(): string {
  const m = credentials.getConfiguredMethod();
  if (m === null) {
    throw new Error("github-tools is unauthenticated; run `particle build` to set up oauth or a PAT");
  }
  return m;
}

// Convenience wrapper: every tool reads the configured credential,
// sets the recommended Accept header, and bubbles non-2xx as a
// thrown error so the runtime returns a HandlerError with the
// API's message.
async function gh(path: string, init: RequestInit = {}): Promise<unknown> {
  const fetcher = await credentials.fetcher(authMethod());
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/vnd.github+json");
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const res = await fetcher(`https://api.github.com${path}`, { ...init, headers });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`GitHub API ${res.status} ${res.statusText}: ${text}`);
  }
  return res.json();
}

export default {
  name: "github-tools",
  description: "Read repos and issues; open new issues.",
  version: "0.1.0",

  capabilities: {
    http: { allowedHosts: ["api.github.com"] },
    credentials: {
      required: true,
      methods: {
        // OAuth: full account-level access. Best for interactive
        // users; the device-code flow makes it work even without a
        // local browser.
        oauth: {
          type: "oauth2",
          description: "Sign in to GitHub via OAuth",
          flows: ["authorization-code", "device-code"],
          scopes: ["repo", "read:user"],
          provider: "github",
        },
        // PAT: a fine-grained or classic personal access token,
        // sent as `Authorization: Bearer <token>`. Best for CI /
        // headless setups where a browser-based OAuth round-trip
        // is awkward. The manifest pre-sets the location, so
        // setup only asks for the value — no "where does this
        // key go?" prompt.
        pat: {
          type: "apikey",
          description: "Use a personal access token",
          location: { kind: "auth-scheme", scheme: "Bearer" },
        },
      },
    },
  },

  tools: {
    get_repo: {
      description: "Fetch a repository's metadata.",
      inputSchema: {
        type: "object",
        properties: {
          owner: { type: "string", description: "Owning user or organization." },
          repo:  { type: "string", description: "Repository name." },
        },
        required: ["owner", "repo"],
      },
      handler: async ({ owner, repo }: { owner: string; repo: string }) => {
        const r = (await gh(`/repos/${owner}/${repo}`)) as Record<string, unknown>;
        return {
          full_name:   r.full_name,
          description: r.description,
          stars:       r.stargazers_count,
          open_issues: r.open_issues_count,
          default_branch: r.default_branch,
          url:         r.html_url,
        };
      },
    },

    list_issues: {
      description: "List issues for a repository.",
      inputSchema: {
        type: "object",
        properties: {
          owner: { type: "string" },
          repo:  { type: "string" },
          state: {
            type: "string",
            description: "Filter by state.",
            enum: ["open", "closed", "all"],
            default: "open",
          },
          per_page: {
            type: "integer",
            description: "Max number of issues to return (1-100).",
            default: 30,
          },
        },
        required: ["owner", "repo"],
      },
      handler: async (
        { owner, repo, state = "open", per_page = 30 }:
        { owner: string; repo: string; state?: string; per_page?: number },
      ) => {
        const q = new URLSearchParams({ state, per_page: String(per_page) });
        const issues = (await gh(`/repos/${owner}/${repo}/issues?${q}`)) as Array<Record<string, unknown>>;
        // GitHub's /issues endpoint also returns PRs (each carries a
        // `pull_request` field) — strip them out so the tool's name
        // matches its behavior.
        return issues
          .filter((i) => i.pull_request === undefined)
          .map((i) => ({
            number: i.number,
            title:  i.title,
            state:  i.state,
            author: (i.user as Record<string, unknown> | null)?.login ?? null,
            url:    i.html_url,
          }));
      },
    },

    create_issue: {
      description: "Open a new issue on a repository.",
      inputSchema: {
        type: "object",
        properties: {
          owner: { type: "string" },
          repo:  { type: "string" },
          title: { type: "string", description: "Issue title." },
          body:  { type: "string", description: "Issue body in GitHub-flavored Markdown." },
        },
        required: ["owner", "repo", "title"],
      },
      handler: async (
        { owner, repo, title, body }:
        { owner: string; repo: string; title: string; body?: string },
      ) => {
        const issue = (await gh(`/repos/${owner}/${repo}/issues`, {
          method: "POST",
          body: JSON.stringify({ title, body }),
        })) as Record<string, unknown>;
        return { number: issue.number, url: issue.html_url };
      },
    },
  },

  // Probe the credential by hitting /user. Returns the authenticated
  // login on success; degrades to "unhealthy" if GitHub rejects the
  // token (refresh likely failed or the user revoked the grant).
  ping: async () => {
    const method = credentials.getConfiguredMethod();
    if (method === null) {
      return { status: "unhealthy" as const, message: "no credential configured" };
    }
    const fetcher = await credentials.fetcher(method);
    const res = await fetcher("https://api.github.com/user");
    if (res.ok) {
      const u = (await res.json()) as { login?: string };
      return { status: "ok" as const, message: `authenticated as ${u.login ?? "unknown"} via ${method}` };
    }
    return {
      status: "unhealthy" as const,
      message: `GitHub /user returned ${res.status}`,
    };
  },
};
