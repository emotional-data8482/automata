package core

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
	case ProviderRequestPreparedPayload:
		v.Request = cloneRequest(v.Request)
		return v
	case ProviderResponseAcceptedPayload:
		v.Response.Message = cloneMessages([]Message{v.Response.Message})[0]
		v.Diagnostics = cloneDiagnostics(v.Diagnostics)
		return v
	case ToolBatchStartedPayload:
		v.Calls = append([]ToolCallData(nil), v.Calls...)
		for i := range v.Calls {
			v.Calls[i] = cloneCallData(v.Calls[i])
		}
		return v
	case ToolCallRequestedPayload:
		v.Call = cloneCallData(v.Call)
		return v
	case ToolCallFinishedPayload:
		v.Call = cloneCallData(v.Call)
		v.Result.Blocks = cloneBlocks(v.Result.Blocks)
		return v
	case ToolBatchFinishedPayload:
		v.Results = append([]ToolCallFinishedPayload(nil), v.Results...)
		for i := range v.Results {
			v.Results[i] = cloneEventPayload(v.Results[i]).(ToolCallFinishedPayload)
		}
		return v
	case TurnFinishedPayload:
		if v.Response != nil {
			r := *v.Response
			r.Message = cloneMessages([]Message{r.Message})[0]
			v.Response = &r
		}
		v.Results = cloneEventPayload(ToolBatchFinishedPayload{Results: v.Results}).(ToolBatchFinishedPayload).Results
		return v
	default:
		return p
	}
}
func cloneCallData(c ToolCallData) ToolCallData {
	c.Requested = cloneBlock(c.Requested).(ToolUseBlock)
	c.Effective = cloneBlock(c.Effective).(ToolUseBlock)
	return c
}
