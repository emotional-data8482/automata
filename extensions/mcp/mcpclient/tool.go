package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/emotional-data8482/automata/core"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// remoteTool is one server tool. definition carries the advertised name and
// the lowered schema; remoteName is what the server is called with.
type remoteTool struct {
	client     *Client
	remoteName string
	definition core.ToolDefinition
}

func (t *remoteTool) Definition() core.ToolDefinition {
	d := t.definition
	d.InputSchema = append(json.RawMessage(nil), d.InputSchema...)
	return d
}

func (t *remoteTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolResult, error) {
	params := &mcp.CallToolParams{Name: t.remoteName}
	if len(args) > 0 && string(args) != "null" {
		params.Arguments = args
	}
	result, err := t.client.session.CallTool(ctx, params)
	if err != nil {
		if ctx.Err() != nil {
			return core.ToolResult{}, ctx.Err()
		}
		if answer := serverAnswer(err); answer != nil {
			return core.ErrorResult(fmt.Sprintf("mcp server error %d: %s", answer.Code, answer.Message)), nil
		}
		return core.ToolResult{}, fmt.Errorf("mcp: call %q: %w", t.remoteName, err)
	}
	return toolResult(result), nil
}

// connectionCodes are the SDK's own jsonrpc2 codes for failures in which no
// server answer arrived: unknown error, client closing, server closing, and
// rejected by transport (network errors and transient HTTP statuses). They
// are unexported in the SDK's internal/jsonrpc2/wire.go.
var connectionCodes = map[int64]bool{-32001: true, -32003: true, -32004: true, -32005: true}

// serverAnswer finds the JSON-RPC error the server responded with in err's
// chain. The SDK also reports its own connection failures as wire errors,
// sometimes wrapping a server's answer, so every wrapped error is examined.
func serverAnswer(err error) *jsonrpc.Error {
	if wire, ok := err.(*jsonrpc.Error); ok && wire != nil && !connectionCodes[wire.Code] {
		return wire
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		if inner := wrapped.Unwrap(); inner != nil {
			return serverAnswer(inner)
		}
	case interface{ Unwrap() []error }:
		for _, inner := range wrapped.Unwrap() {
			if answer := serverAnswer(inner); answer != nil {
				return answer
			}
		}
	}
	return nil
}
