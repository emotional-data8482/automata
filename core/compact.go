package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// CompactorConfig configures the [Compactor] pre-send hook.
type CompactorConfig struct {
	// TriggerTokens is the estimated context size (in tokens) at or above which
	// compaction kicks in. Estimation is a cheap character-based heuristic
	// (~4 chars/token over the serialized transcript), so treat it as
	// approximate. Required; a value <= 0 disables the compactor.
	TriggerTokens int
	// KeepRecent is the number of trailing messages kept verbatim (never
	// summarized). Defaults to 8.
	KeepRecent int
	// Prompt overrides the summarization instruction sent to the summarizer.
	Prompt string
	// MinRecompute bounds how often the summary is regenerated: the summary is
	// reused until this many new messages have accrued past what it already
	// covers, at which point it is recomputed. Defaults to KeepRecent. Larger
	// values summarize less often (cheaper) at the cost of a larger verbatim
	// "pending" region in each request.
	MinRecompute int
}

const defaultCompactPrompt = "You are a conversation summarizer for an AI agent. " +
	"Summarize the transcript below into a compact brief that preserves the task, " +
	"key facts and data discovered, decisions made, tool results that matter, and " +
	"any open threads or next steps. Be faithful and concise; omit pleasantries."

// Compactor returns a [PreSendHook] that keeps a long conversation within a
// token budget by replacing older turns with an LLM-generated summary while
// leaving the system prompt and the most recent turns intact. It uses p to
// generate the summary.
//
// The hook transforms only the view sent to the provider — it never mutates the
// canonical transcript (per the [PreSendHook] contract), so [Session.Messages]
// still returns the full history. It never cuts between an assistant tool call
// and its tool result. The summary is memoized (keyed by the content it
// covers) and regenerated only every CompactorConfig.MinRecompute messages, so
// a shared compactor is safe across concurrent sessions and does not pay a
// summarization call every turn.
func Compactor(p Provider, cfg CompactorConfig) PreSendHook {
	c := &compactor{p: p, cfg: cfg}
	return c.hook
}

type compactor struct {
	p   Provider
	cfg CompactorConfig

	mu              sync.Mutex
	haveSummary     bool
	summary         string
	summarizedCount int    // number of post-system messages the summary covers
	coveredHash     string // hash of those covered messages
}

func (c *compactor) hook(ctx context.Context, messages []Message, tools []Tool) ([]Message, []Tool, error) {
	if c.cfg.TriggerTokens <= 0 || estimateTokens(messages) < c.cfg.TriggerTokens {
		return messages, tools, nil
	}

	keep := c.cfg.KeepRecent
	if keep <= 0 {
		keep = 8
	}
	margin := c.cfg.MinRecompute
	if margin <= 0 {
		margin = keep
	}

	systemEnd := 0
	for systemEnd < len(messages) && messages[systemEnd].Role == "system" {
		systemEnd++
	}

	cut := len(messages) - keep
	if cut <= systemEnd {
		return messages, tools, nil // nothing older than the recent window to summarize
	}
	// Never cut between an assistant tool_use and its tool_result: advance the
	// cut forward until the kept suffix begins on a non-tool message, which pulls
	// any dangling tool results into the summarized prefix.
	for cut < len(messages) && messages[cut].Role == "tool" {
		cut++
	}
	if cut >= len(messages) {
		return messages, tools, nil // only tool results remain; nothing safe to keep
	}

	prefixLen := cut - systemEnd

	// Decide whether the cached summary can be reused (same conversation, and not
	// yet grown past the recompute margin).
	c.mu.Lock()
	canReuse := c.haveSummary &&
		c.summarizedCount <= prefixLen &&
		prefixLen-c.summarizedCount < margin &&
		c.coveredHash == hashMessages(messages[systemEnd:systemEnd+c.summarizedCount])
	cachedSummary, cachedCount := c.summary, c.summarizedCount
	c.mu.Unlock()

	var summary string
	var summarizedCount int
	if canReuse {
		summary, summarizedCount = cachedSummary, cachedCount
	} else {
		s, err := c.summarize(ctx, messages[systemEnd:cut])
		if err != nil {
			return nil, nil, fmt.Errorf("compactor summarize: %w", err)
		}
		summary, summarizedCount = s, prefixLen
		c.mu.Lock()
		c.haveSummary = true
		c.summary = s
		c.summarizedCount = prefixLen
		c.coveredHash = hashMessages(messages[systemEnd:cut])
		c.mu.Unlock()
	}

	// pending = messages covered by neither the summary nor the recent window;
	// kept verbatim until the next recompute folds them into the summary.
	pending := messages[systemEnd+summarizedCount : cut]
	suffix := messages[cut:]

	rest := make([]Message, 0, len(pending)+len(suffix))
	rest = append(rest, pending...)
	rest = append(rest, suffix...)

	summaryText := "Summary of the earlier conversation:\n" + summary

	out := make([]Message, 0, systemEnd+1+len(rest))
	out = append(out, messages[:systemEnd]...)
	if len(rest) > 0 && rest[0].Role == "user" {
		// Merge the summary into the first user message so roles keep
		// alternating (a lone summary user message before another user message
		// would be two user turns in a row).
		first := rest[0]
		first.Blocks = append(Blocks{TextBlock{Text: summaryText + "\n\n"}}, first.Blocks...)
		out = append(out, first)
		out = append(out, rest[1:]...)
	} else {
		out = append(out, UserMessage(summaryText))
		out = append(out, rest...)
	}
	return out, tools, nil
}

func (c *compactor) summarize(ctx context.Context, prefix []Message) (string, error) {
	prompt := c.cfg.Prompt
	if prompt == "" {
		prompt = defaultCompactPrompt
	}
	resp, err := c.p.Invoke(ctx, Request{Messages: []Message{
		SystemMessage(prompt),
		UserMessage("Conversation transcript to summarize:\n\n" + renderTranscript(prefix)),
	}})
	if err != nil {
		return "", err
	}
	return resp.Message.Text(), nil
}

// estimateTokens approximates the token size of a message list. It prefers the
// most recent reported [Usage] (real provider numbers) and falls back to a
// ~4-chars-per-token heuristic over the serialized transcript when no usage is
// available (e.g. a freshly resumed session).
func estimateTokens(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if u := messages[i].Usage; u != nil {
			return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
		}
	}
	blob, err := json.Marshal(messages)
	if err != nil {
		return 0
	}
	return len(blob) / 4
}

// renderTranscript flattens messages into a plain-text transcript for the
// summarizer: one line per message with its role, text, tool calls, and tool
// results.
func renderTranscript(messages []Message) string {
	var b strings.Builder
	for _, m := range messages {
		fmt.Fprintf(&b, "[%s]", m.Role)
		if t := m.Text(); t != "" {
			b.WriteByte(' ')
			b.WriteString(t)
		}
		for _, tu := range m.ToolUses() {
			fmt.Fprintf(&b, " (called %s with %s)", tu.Name, string(tu.Input))
		}
		for _, blk := range m.Blocks {
			if tr, ok := blk.(ToolResultBlock); ok {
				fmt.Fprintf(&b, " (result: %s)", Message{Blocks: tr.Content}.Text())
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func hashMessages(messages []Message) string {
	blob, _ := json.Marshal(messages)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}
