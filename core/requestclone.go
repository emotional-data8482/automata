package core

import "encoding/json"

func cloneMessages(messages []Message) []Message {
	out := append([]Message(nil), messages...)
	for i := range out {
		out[i].Blocks = cloneBlocks(out[i].Blocks)
		if out[i].Usage != nil {
			v := *out[i].Usage
			out[i].Usage = &v
		}
	}
	return out
}
func cloneBlocks(blocks Blocks) Blocks {
	if blocks == nil {
		return nil
	}
	out := make(Blocks, len(blocks))
	for i, b := range blocks {
		out[i] = cloneBlock(b)
	}
	return out
}
func cloneBlock(b Block) Block {
	b = blockValue(b)
	switch v := b.(type) {
	case ToolUseBlock:
		v.Input = append([]byte(nil), v.Input...)
		return v
	case ToolResultBlock:
		v.Content = cloneBlocks(v.Content)
		return v
	case ImageBlock:
		v.Data = append([]byte(nil), v.Data...)
		return v
	case RawBlock:
		v.Data = append([]byte(nil), v.Data...)
		return v
	case TextBlock, ThinkingBlock:
		return b
	case nil:
		return nil
	default:
		panic("unsupported block")
	}
}

func cloneDiagnostics(ds []RunDiagnostic) []RunDiagnostic {
	out := append([]RunDiagnostic(nil), ds...)
	for i := range out {
		out[i].Data = append([]byte(nil), out[i].Data...)
	}
	return out
}
func cloneRunResult(r RunResult) RunResult {
	r.Messages = cloneMessages(r.Messages)
	r.FinalMessage = cloneMessages([]Message{r.FinalMessage})[0]
	r.Diagnostics = cloneDiagnostics(r.Diagnostics)
	r.terminalToolInput = append([]byte(nil), r.terminalToolInput...)
	r.StructuredOutput = append(json.RawMessage(nil), r.StructuredOutput...)
	return r
}
func cloneStreamEvent(e StreamEvent) StreamEvent {
	e.ToolCall = cloneBlock(e.ToolCall).(ToolUseBlock)
	e.ResultBlocks = cloneBlocks(e.ResultBlocks)
	if e.Usage != nil {
		v := *e.Usage
		e.Usage = &v
	}
	return e
}
func cloneEventPayload(p RunEventPayload) RunEventPayload {
	switch v := p.(type) {
	case RunFinishedPayload:
		v.Result = cloneRunResult(v.Result)
		return v
	case CheckpointCommittedPayload:
		v.Checkpoint.Messages = cloneMessages(v.Checkpoint.Messages)
		return v
	default:
		return p
	}
}
