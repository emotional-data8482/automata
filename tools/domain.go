package tools

import (
	"context"
	"github.com/emotional-data8482/automata/core"
)

// domainTool deliberately reports first-party validation, I/O, and tool-owned
// timeout failures to the model. Cancellation of its parent remains fatal.
func domainTool[P any](name, description string, handler func(context.Context, P) (string, error)) core.Tool {
	return core.FuncResult(name, description, func(ctx context.Context, p P) (core.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return core.ToolResult{}, err
		}
		text, err := handler(ctx, p)
		if err != nil {
			if ctx.Err() != nil {
				return core.ToolResult{}, ctx.Err()
			}
			return core.ErrorResult(err.Error()), nil
		}
		return core.TextResult(text), nil
	})
}
