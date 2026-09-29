package mcpclient

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/emotional-data8482/automata/core"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolResult converts a completed call. Content blocks are authoritative;
// structured content is rendered only when the server sent no content, since
// servers that return both are expected to mirror it as JSON text.
func toolResult(result *mcp.CallToolResult) core.ToolResult {
	blocks := make(core.Blocks, 0, len(result.Content))
	for _, content := range result.Content {
		blocks = append(blocks, contentBlock(content))
	}
	if len(blocks) == 0 && result.StructuredContent != nil {
		text := "[structured content omitted: not JSON-encodable]"
		if raw, err := json.Marshal(result.StructuredContent); err == nil {
			text = string(raw)
		}
		blocks = append(blocks, core.TextBlock{Text: text})
	}
	return core.ToolResult{Blocks: blocks, IsError: result.IsError}
}

// contentBlock maps one MCP content item to the closest core block. Content
// core cannot carry becomes a bracketed placeholder so the model sees that
// something was omitted.
func contentBlock(content mcp.Content) core.Block {
	switch c := content.(type) {
	case *mcp.TextContent:
		return core.TextBlock{Text: c.Text}
	case *mcp.ImageContent:
		return core.ImageBlock{MediaType: c.MIMEType, Data: c.Data}
	case *mcp.AudioContent:
		return core.TextBlock{Text: fmt.Sprintf("[audio omitted: %s, %d bytes]", mimeOrUnknown(c.MIMEType), len(c.Data))}
	case *mcp.ResourceLink:
		var b strings.Builder
		fmt.Fprintf(&b, "[resource link: %s", c.URI)
		if c.Name != "" {
			fmt.Fprintf(&b, " name=%q", c.Name)
		}
		if c.MIMEType != "" {
			fmt.Fprintf(&b, " type=%s", c.MIMEType)
		}
		if c.Description != "" {
			fmt.Fprintf(&b, " description=%q", c.Description)
		}
		b.WriteByte(']')
		return core.TextBlock{Text: b.String()}
	case *mcp.EmbeddedResource:
		return embeddedBlock(c.Resource)
	case nil:
		return core.TextBlock{Text: "[empty MCP content omitted]"}
	default:
		return core.TextBlock{Text: fmt.Sprintf("[unsupported MCP content omitted: %T]", content)}
	}
}

func embeddedBlock(resource *mcp.ResourceContents) core.Block {
	switch {
	case resource == nil:
		return core.TextBlock{Text: "[empty MCP resource omitted]"}
	case resource.Blob == nil:
		return core.TextBlock{Text: fmt.Sprintf("[resource: %s]\n%s", resource.URI, resource.Text)}
	case strings.HasPrefix(resource.MIMEType, "image/"):
		return core.ImageBlock{MediaType: resource.MIMEType, Data: resource.Blob}
	default:
		return core.TextBlock{Text: fmt.Sprintf("[embedded resource omitted: %s, %s, %d bytes]",
			resource.URI, mimeOrUnknown(resource.MIMEType), len(resource.Blob))}
	}
}

func mimeOrUnknown(mimeType string) string {
	if mimeType == "" {
		return "unknown type"
	}
	return mimeType
}
