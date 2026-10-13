package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/emotional-data8482/automata/core"
)

// checkpointTool pauses a run at a CAPTCHA or other challenge for a user
// decision.
const checkpointTool = "browser_checkpoint"

type checkpointParams struct {
	Note string `json:"note" desc:"what the browser is stuck on, such as 'image-grid CAPTCHA blocks the login form'"`
}

// checkpointTarget extracts the wait target. It never fails: Runtime treats
// a target error as fatal, so an unusable note is shown as-is.
func checkpointTarget(raw json.RawMessage) (string, error) {
	var p checkpointParams
	_ = json.Unmarshal(raw, &p)
	if s := strings.TrimSpace(p.Note); s != "" {
		return clip(s), nil
	}
	return "(no note given)", nil
}

// checkpoint returns the browser_checkpoint tool: a durable Runtime approval
// whose body runs only after the user allows it. The user deals with the
// challenge in the visible browser window, then approves to continue the
// run; a denial reaches the model as an ordinary error.
func (s *Session) checkpoint() core.Tool {
	const description = "Pause and ask the user to deal with a CAPTCHA or similar challenge in the visible browser window. Call it instead of retrying or trying to bypass the challenge. The user solves it and approves to continue; then take a fresh browser_snapshot and confirm the page now shows the expected content."
	tool := core.WithToolEffectPolicy(core.FuncResult(checkpointTool, description,
		func(ctx context.Context, p checkpointParams) (core.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return core.ToolResult{}, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.closed {
				return notApplied("browser session is closed"), nil
			}
			if _, ok := core.ToolOperationFromContext(ctx); !ok {
				// Only Runtime can obtain the user decision. Called any other
				// way, nobody dealt with the challenge, so refuse rather than
				// claim a pause happened.
				return notApplied(checkpointTool + " needs a user decision: run the agent under a core.Runtime with an approval authorizer"), nil
			}
			return core.TextResult("The user says the challenge is dealt with. Take a fresh browser_snapshot before acting, and confirm the page now shows the expected content."), nil
		}), core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly})
	return core.WithDurableWait(tool, core.DurableWaitPolicy{
		Kind: core.WaitApproval,
		Prompt: func(raw json.RawMessage) (string, error) {
			var p checkpointParams
			_ = json.Unmarshal(raw, &p)
			note := strings.TrimSpace(p.Note)
			if note == "" {
				note = "a CAPTCHA or access challenge"
			}
			return fmt.Sprintf("The browser hit %s. Solve it in the visible browser window, then approve to continue.", clip(note)), nil
		},
		Target: checkpointTarget,
	})
}
