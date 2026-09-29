// Package mcpserver serves automata tools to Model Context Protocol clients
// through the official MCP Go SDK. The SDK server owns transports (stdio,
// streamable HTTP), sessions, and capability negotiation; this package
// registers [core.Tool] values on it.
//
//	server := mcp.NewServer(&mcp.Implementation{Name: "files", Version: "v1"}, nil)
//	if err := mcpserver.AddTools(server, tools.ReadFile(root)); err != nil { … }
//	err := server.Run(ctx, &mcp.StdioTransport{})
//
// Each call validates its arguments against the tool's input schema, as a
// Runtime does, and reports a violation as an isError result the client's
// model can correct. The tool then executes directly: Runtime policies do not
// apply. There is no ToolPolicy timeout, call budget, or rate limit, no
// durable approval or question wait, and no effect guard or recovery. Export
// only tools that are safe to call that way, and authenticate the transport
// (for example, in the HTTP handler in front of the streamable handler).
//
// Results: an [core.ErrorResult] becomes isError content. A Go error becomes
// a JSON-RPC internal error, and cancellation is returned unchanged. Text and
// inline images map to MCP content, and URL images become resource links.
// Other blocks degrade to a bracketed placeholder line. Effect reports have
// no MCP representation and are not sent.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/emotional-data8482/automata/core"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AddTools registers tools on server. It validates every tool before
// registering any: names must be non-empty and distinct, and each input
// schema must be an object schema. A tool already registered on server under
// the same name is replaced, as [mcp.Server.AddTool] does.
func AddTools(server *mcp.Server, tools ...core.Tool) error {
	if server == nil {
		return errors.New("mcp: nil server")
	}
	type entry struct {
		tool       core.Tool
		definition core.ToolDefinition
		schema     *jsonschema.Resolved
	}
	entries := make([]entry, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool == nil || (reflect.ValueOf(tool).Kind() == reflect.Pointer && reflect.ValueOf(tool).IsNil()) {
			return errors.New("mcp: nil tool")
		}
		definition := tool.Definition()
		if definition.Name == "" {
			return errors.New("mcp: tool name is empty")
		}
		if seen[definition.Name] {
			return fmt.Errorf("mcp: duplicate tool %q", definition.Name)
		}
		seen[definition.Name] = true
		schema, err := resolveSchema(definition.InputSchema)
		if err != nil {
			return fmt.Errorf("mcp: tool %q: %w", definition.Name, err)
		}
		entries = append(entries, entry{tool, definition, schema})
	}
	for _, e := range entries {
		server.AddTool(&mcp.Tool{
			Name:        e.definition.Name,
			Description: e.definition.Description,
			InputSchema: e.definition.InputSchema,
		}, handler(e.tool, e.schema))
	}
	return nil
}

// resolveSchema prepares a tool's input schema for argument validation.
func resolveSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	if schema.Type != "object" {
		return nil, errors.New("input schema must be an object schema")
	}
	// core does not interpret $schema; validate under the SDK's dialect.
	schema.Schema = ""
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	return resolved, nil
}

func handler(tool core.Tool, schema *jsonschema.Resolved) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.Params.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		var instance any
		if err := json.Unmarshal(args, &instance); err != nil {
			return errorResult("invalid arguments: " + err.Error()), nil
		}
		if err := schema.Validate(instance); err != nil {
			return errorResult("invalid arguments: " + err.Error()), nil
		}
		result, err := tool.Execute(ctx, args)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
		}
		return callToolResult(result), nil
	}
}

func errorResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

// callToolResult converts a tool result. An empty result is sent as one
// empty text item, as core normalizes it for providers.
func callToolResult(result core.ToolResult) *mcp.CallToolResult {
	content := make([]mcp.Content, 0, len(result.Blocks))
	for _, block := range result.Blocks {
		content = append(content, contentItem(block))
	}
	if len(content) == 0 {
		content = append(content, &mcp.TextContent{})
	}
	return &mcp.CallToolResult{Content: content, IsError: result.IsError}
}

func contentItem(block core.Block) mcp.Content {
	switch b := block.(type) {
	case core.TextBlock:
		return &mcp.TextContent{Text: b.Text}
	case core.ImageBlock:
		if b.URL != "" && len(b.Data) == 0 {
			return &mcp.ResourceLink{URI: b.URL, Name: "image", MIMEType: b.MediaType}
		}
		return &mcp.ImageContent{MIMEType: b.MediaType, Data: b.Data}
	case core.RawBlock:
		return &mcp.TextContent{Text: fmt.Sprintf("[unsupported tool result block omitted: raw %s/%s]", b.Provider, b.Type)}
	default:
		return &mcp.TextContent{Text: fmt.Sprintf("[unsupported tool result block omitted: %T]", block)}
	}
}
