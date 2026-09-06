package tools

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/emotional-data8482/automata/core"
	"testing"
)

// Existing content/security fixtures use this text view after asserting that
// domain failures use rich error results, never fatal Go errors.
func executeDomain(t *testing.T, tool core.Tool, ctx context.Context, args string) (string, error) {
	t.Helper()
	result, err := tool.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("unexpected fatal domain error: %v", err)
	}
	if result.IsError {
		return result.Text(), errors.New(result.Text())
	}
	return result.Text(), nil
}

func TestDomainToolsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tool := range []core.Tool{HTTPFetch(), ReadFile(t.TempDir()), WriteFile(t.TempDir()), Shell(ShellConfig{}), WebSearch(&fakeSearcher{})} {
		_, err := tool.Execute(ctx, json.RawMessage("{}"))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s error=%v", tool.Definition().Name, err)
		}
	}
}
