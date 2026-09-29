// Package mcpclient exposes the tools of a Model Context Protocol server as
// [core.Tool] values. The official MCP Go SDK owns transports, framing,
// authentication, and capability negotiation; this package translates tool
// definitions, arguments, results, and errors at that boundary.
//
//	client, err := mcpclient.Connect(ctx,
//		&mcp.CommandTransport{Command: exec.Command("mcp-server-fetch")},
//		mcpclient.Options{})
//	defer client.Close()
//	tools, err := client.Tools(ctx, mcpclient.ToolOptions{Prefix: "fetch_"})
//	agent, err := core.New(provider, core.AgentConfig{Tools: tools})
//
// Error mapping: a result the server marks isError, and a JSON-RPC error the
// server answers with (an unknown tool, invalid parameters, an internal
// error), become model-visible error results the run can recover from.
// Cancellation of the call's context is returned unchanged, and the SDK
// notifies the server. Any other failure is a Go error and therefore fatal to
// the run: a closed connection, an expired session, a network error, or a
// transient HTTP status (429, 5xx) on the streamable transport. Nothing is
// marked retryable: a remote tool may have side effects.
//
// Schemas: core enforces a subset of JSON Schema and rejects keywords it
// cannot honor, while MCP servers publish arbitrary JSON Schema. Each input
// schema is rewritten into the subset. Local $ref pointers are inlined,
// nullable anyOf/oneOf unions collapse to nullable types, const becomes a
// one-value enum, and allOf branches merge. Assertions core cannot express
// (minItems, multi-branch unions, patterns Go's regexp cannot compile, …) are
// dropped. That relaxes only core's local pre-check: the server remains the
// authoritative validator and reports violations as recoverable tool errors.
//
// Progress: calls do not request progress notifications yet. Core has no
// progress stream event for tools to report into; one is planned.
//
// Content: text and images map to core blocks, and embedded image resources
// become images. Structured content is rendered as JSON text when the server
// sends no content blocks. Audio, resource links, and embedded non-image
// binary resources degrade to a bracketed placeholder line.
//
// Policy: remote tools are unclassified ([core.ToolEffectLegacy]) and carry
// no approval requirement. Wrap them with [core.WithToolEffectPolicy] or
// [core.WithDurableWait] to declare effects or require approval; server
// annotations are hints from the server, not a trust decision.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"

	"github.com/emotional-data8482/automata/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const modulePath = "github.com/emotional-data8482/automata/extensions/mcp"

// Options configures [Connect]. The zero value is valid.
type Options struct {
	// Implementation identifies this client to the server. Nil selects
	// "automata" at this module's version.
	Implementation *mcp.Implementation
	// ClientOptions configures the SDK client (sampling and elicitation
	// handlers, capabilities, keepalive, logging).
	ClientOptions *mcp.ClientOptions
}

// Client is a connected MCP client session. It is safe for concurrent use,
// and so are the tools it returns.
type Client struct {
	session *mcp.ClientSession
}

// Connect starts a session with the server behind transport. Use the SDK's
// transports: [mcp.CommandTransport] for a local subprocess over stdio,
// [mcp.StreamableClientTransport] for a remote server (its HTTPClient
// carries authentication), or [mcp.NewInMemoryTransports] in tests.
func Connect(ctx context.Context, transport mcp.Transport, opts Options) (*Client, error) {
	if transport == nil {
		return nil, errors.New("mcp: nil transport")
	}
	impl := opts.Implementation
	if impl == nil {
		impl = &mcp.Implementation{Name: "automata", Version: moduleVersion()}
	}
	session, err := mcp.NewClient(impl, opts.ClientOptions).Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w", err)
	}
	return &Client{session: session}, nil
}

// Session returns the SDK session, for protocol features this package does
// not wrap (resources, prompts, completion).
func (c *Client) Session() *mcp.ClientSession { return c.session }

// Close ends the session. Calls through its tools then fail with a Go error.
func (c *Client) Close() error { return c.session.Close() }

// ToolOptions selects and names the tools [Client.Tools] returns.
type ToolOptions struct {
	// Prefix is prepended to each advertised tool name, so tools from several
	// servers cannot collide. Calls use the server's own name.
	Prefix string
	// Include, when non-empty, exposes only these server tools, named as the
	// server lists them. Listing a tool the server does not offer is an
	// error.
	Include []string
}

// Tools lists the server's tools, following pagination, and returns them in
// server order. The result is a snapshot: an Agent's tools are frozen, so a
// server that changes its tool list needs a new Agent revision.
func (c *Client) Tools(ctx context.Context, opts ToolOptions) ([]core.Tool, error) {
	include := make(map[string]bool, len(opts.Include))
	for _, name := range opts.Include {
		include[name] = false
	}
	var tools []core.Tool
	for tool, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}
		if len(opts.Include) > 0 {
			if _, ok := include[tool.Name]; !ok {
				continue
			}
			include[tool.Name] = true
		}
		schema, err := lowerSchema(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: tool %q: %w", tool.Name, err)
		}
		tools = append(tools, &remoteTool{client: c, remoteName: tool.Name, definition: core.ToolDefinition{
			Name:        opts.Prefix + tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		}})
	}
	var missing []string
	for name, found := range include {
		if !found {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("mcp: server does not offer tools %q", missing)
	}
	return tools, nil
}

func moduleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == modulePath {
				return dep.Version
			}
		}
	}
	return "(devel)"
}
