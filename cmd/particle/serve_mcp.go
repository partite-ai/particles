package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	regsqlite "github.com/partite-ai/particles/registry/sqlite"
	"github.com/partite-ai/particles/runtime"
)

func newServeMCPCmd() *cobra.Command {
	var (
		dbPath   string
		includes []string
		excludes []string
	)
	cmd := &cobra.Command{
		Use:   "serve-mcp <name>[@version]",
		Short: "Run a stdio MCP server backed by a registered particle",
		Long: `Run a stdio MCP server backed by the registered particle.
Exposes every tool the particle defines to any MCP client, unless
--only-tools or --exclude-tools narrows the set.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServeMCP(cmd, args[0], dbPath, includes, excludes)
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "", dbFlagUsage())
	cmd.Flags().StringSliceVar(&includes, "only-tools", nil,
		"Comma-separated allowlist: expose only the named tools. Mutually exclusive with --exclude-tools.")
	cmd.Flags().StringSliceVar(&excludes, "exclude-tools", nil,
		"Comma-separated denylist: expose every tool except the named ones.")
	cmd.MarkFlagsMutuallyExclusive("only-tools", "exclude-tools")
	return cmd
}

func runServeMCP(cmd *cobra.Command, target, dbPath string, includes, excludes []string) error {
	dbPath, err := resolveDBPath(dbPath)
	if err != nil {
		return err
	}

	// Cancel on SIGINT/SIGTERM so an MCP client disconnect tears
	// the runtime down cleanly; otherwise wasm cleanup waits on
	// the (now-empty) stdin pipe.
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	reg, err := regsqlite.New(ctx, db)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	entry, err := resolveParticle(ctx, reg, target)
	if err != nil {
		return err
	}

	p, teardown, err := bootParticle(ctx, db, entry, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	defer teardown()

	tools, err := p.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	tools, err = filterTools(tools, includes, excludes)
	if err != nil {
		return err
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    entry.Name,
		Version: entry.Version,
	}, nil)

	for _, td := range tools {
		// Register each particle tool with its raw JSON-Schema
		// input definition. The runtime already validated the
		// schema during build (Phase 5: introspect.wasm
		// extracted it from the particle source), so we hand it
		// to the MCP SDK as-is via json.RawMessage.
		mcpTool := &mcp.Tool{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: json.RawMessage(td.InputSchemaJSON),
		}
		toolName := td.Name // capture into closure
		server.AddTool(mcpTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := []byte(req.Params.Arguments)
			if len(args) == 0 {
				// MCP allows omitting `arguments` for
				// no-arg tools; the particle runtime
				// expects a JSON object.
				args = []byte("{}")
			}
			out, err := p.CallTool(ctx, toolName, args)
			if err != nil {
				// Tool-side error: pack into the result
				// with IsError, not as a protocol error,
				// so the LLM client sees and can
				// self-correct.
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
					IsError: true,
				}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
			}, nil
		})
	}

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		// A clean shutdown via SIGINT/SIGTERM cancels ctx; that
		// surfaces as context.Canceled and isn't an exit-code
		// failure.
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// Compile-time guard: serve-mcp depends on runtime.ToolDef having
// the fields we read above.
var _ = func(td runtime.ToolDef) {
	_ = td.Name
	_ = td.Description
	_ = td.InputSchemaJSON
}
