package mcpclient

import (
	"reflect"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestContentBlock(t *testing.T) {
	size := int64(3)
	cases := []struct {
		name string
		in   mcp.Content
		want core.Block
	}{
		{"text", &mcp.TextContent{Text: "hi"}, core.TextBlock{Text: "hi"}},
		{"image", &mcp.ImageContent{MIMEType: "image/png", Data: []byte{1, 2}},
			core.ImageBlock{MediaType: "image/png", Data: []byte{1, 2}}},
		{"audio", &mcp.AudioContent{MIMEType: "audio/wav", Data: []byte{1, 2, 3}},
			core.TextBlock{Text: "[audio omitted: audio/wav, 3 bytes]"}},
		{"audio without type", &mcp.AudioContent{Data: []byte{1}},
			core.TextBlock{Text: "[audio omitted: unknown type, 1 bytes]"}},
		{"resource link", &mcp.ResourceLink{URI: "file:///a.txt", Name: "a", MIMEType: "text/plain", Description: "notes", Size: &size},
			core.TextBlock{Text: `[resource link: file:///a.txt name="a" type=text/plain description="notes"]`}},
		{"bare resource link", &mcp.ResourceLink{URI: "file:///b"}, core.TextBlock{Text: "[resource link: file:///b]"}},
		{"embedded text", &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///a.txt", Text: "body"}},
			core.TextBlock{Text: "[resource: file:///a.txt]\nbody"}},
		{"embedded image", &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///a.png", MIMEType: "image/png", Blob: []byte{9}}},
			core.ImageBlock{MediaType: "image/png", Data: []byte{9}}},
		{"embedded binary", &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///a.pdf", MIMEType: "application/pdf", Blob: []byte{1, 2}}},
			core.TextBlock{Text: "[embedded resource omitted: file:///a.pdf, application/pdf, 2 bytes]"}},
		{"embedded nothing", &mcp.EmbeddedResource{}, core.TextBlock{Text: "[empty MCP resource omitted]"}},
		{"nil", nil, core.TextBlock{Text: "[empty MCP content omitted]"}},
		{"sampling-only content", &mcp.ToolUseContent{Name: "x"}, core.TextBlock{Text: "[unsupported MCP content omitted: *mcp.ToolUseContent]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentBlock(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestToolResult(t *testing.T) {
	cases := []struct {
		name string
		in   *mcp.CallToolResult
		want core.ToolResult
	}{
		{"content in order", &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "a"}, &mcp.ImageContent{MIMEType: "image/png", Data: []byte{1}}}},
			core.ToolResult{Blocks: core.Blocks{core.TextBlock{Text: "a"}, core.ImageBlock{MediaType: "image/png", Data: []byte{1}}}}},
		{"error flag", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "bad"}}, IsError: true},
			core.ToolResult{Blocks: core.Blocks{core.TextBlock{Text: "bad"}}, IsError: true}},
		{"structured content only", &mcp.CallToolResult{StructuredContent: map[string]any{"n": 1.0}},
			core.ToolResult{Blocks: core.Blocks{core.TextBlock{Text: `{"n":1}`}}}},
		{"content wins over structured", &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"n": 1}`}}, StructuredContent: map[string]any{"n": 1.0}},
			core.ToolResult{Blocks: core.Blocks{core.TextBlock{Text: `{"n": 1}`}}}},
		{"unencodable structured", &mcp.CallToolResult{StructuredContent: func() {}},
			core.ToolResult{Blocks: core.Blocks{core.TextBlock{Text: "[structured content omitted: not JSON-encodable]"}}}},
		{"empty", &mcp.CallToolResult{}, core.ToolResult{Blocks: core.Blocks{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolResult(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
