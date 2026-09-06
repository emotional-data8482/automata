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
	case *ToolUseBlock:
		if v == nil {
			return (*ToolUseBlock)(nil)
		}
		c := cloneBlock(*v).(ToolUseBlock)
		return &c
	case *ToolResultBlock:
		if v == nil {
			return (*ToolResultBlock)(nil)
		}
		c := cloneBlock(*v).(ToolResultBlock)
		return &c
	case *ImageBlock:
		if v == nil {
			return (*ImageBlock)(nil)
		}
		c := cloneBlock(*v).(ImageBlock)
		return &c
	case *RawBlock:
		if v == nil {
			return (*RawBlock)(nil)
		}
		c := cloneBlock(*v).(RawBlock)
		return &c
	case *TextBlock:
		if v == nil {
			return (*TextBlock)(nil)
		}
		c := *v
		return &c
	case *ThinkingBlock:
		if v == nil {
			return (*ThinkingBlock)(nil)
		}
		c := *v
		return &c
	case nil:
		return nil
	default:
		panic("unsupported block")
	}
}
