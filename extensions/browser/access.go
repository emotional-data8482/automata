package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/emotional-data8482/automata/core"
)

// requestAccessTool is the name the not-allowed errors point the model to.
const requestAccessTool = "browser_request_access"

// requestsEnabled reports whether the session offers browser_request_access.
func (s *Session) requestsEnabled() bool { return s.cfg.RequestAccess || s.cfg.AskAccess != nil }

func (s *Session) notAllowedError(host string) error {
	if s.requestsEnabled() {
		return fmt.Errorf("host %q is not allowed; call %s with this host and a reason to ask the user for access", host, requestAccessTool)
	}
	return fmt.Errorf("host %q is not allowed; the application must add it to browser.Config.AllowedHosts", host)
}

type accessParams struct {
	Host   string `json:"host" desc:"the host to visit, such as docs.example.com (a URL is accepted; only its host is used)"`
	Reason string `json:"reason" desc:"why the task needs this host, shown to the user"`
}

// accessTarget extracts the host for the approval prompt. It never fails:
// Runtime treats a prompt or target error as fatal, so an invalid host is
// shown as-is and rejected by the tool body after approval.
func accessTarget(raw json.RawMessage) string {
	var p accessParams
	_ = json.Unmarshal(raw, &p)
	if host, err := normalizeHost(p.Host); err == nil {
		return host
	}
	if s := strings.TrimSpace(p.Host); s != "" {
		return s
	}
	return "(no host given)"
}

// requestAccess returns the browser_request_access tool. With
// Config.AskAccess it asks inline. Otherwise it is a durable Runtime
// approval whose body runs only after the user allows the call.
func (s *Session) requestAccess() core.Tool {
	const description = "Ask the user to allow the browser to visit a host that is not allowed yet. Call it when a navigation or link fails because the host is not allowed. If the user allows it, the host stays allowed for the rest of the session; if not, continue without it."
	tool := core.WithToolEffectPolicy(core.FuncResult(requestAccessTool, description,
		func(ctx context.Context, p accessParams) (core.ToolResult, error) {
			host, err := normalizeHost(p.Host)
			if err != nil {
				return notApplied(err.Error()), nil
			}
			if s.cfg.AskAccess != nil {
				ok, err := s.cfg.AskAccess(ctx, host, strings.TrimSpace(p.Reason))
				if ctx.Err() != nil {
					return core.ToolResult{}, ctx.Err()
				}
				if err != nil {
					return notApplied(fmt.Sprintf("could not ask the user about %s: %v", host, err)), nil
				}
				if !ok {
					return notApplied(fmt.Sprintf("denied: the user did not allow %s; continue without it", host)), nil
				}
			} else if _, ok := core.ToolOperationFromContext(ctx); !ok {
				// Only Runtime enforces the approval. Called any other way the
				// approval never happened, so refuse rather than self-grant.
				return notApplied("browser_request_access needs a user decision: run the agent under a core.Runtime with an approval authorizer, or set browser.Config.AskAccess"), nil
			}
			s.allowHost(host)
			return core.TextResult(fmt.Sprintf("%s is now allowed for this session.", host)), nil
		}), core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly})
	if s.cfg.AskAccess != nil {
		return tool
	}
	return core.WithDurableWait(tool, core.DurableWaitPolicy{
		Kind: core.WaitApproval,
		Prompt: func(raw json.RawMessage) (string, error) {
			var p accessParams
			_ = json.Unmarshal(raw, &p)
			prompt := fmt.Sprintf("Allow the browser to visit %s?", accessTarget(raw))
			if reason := strings.TrimSpace(p.Reason); reason != "" {
				prompt += " Reason: " + clip(reason)
			}
			return prompt, nil
		},
		Target: func(raw json.RawMessage) (string, error) { return accessTarget(raw), nil },
	})
}

// normalizeHost reduces a host or URL to a lowercase host name without port.
// Wildcards are rejected: the user approves one exact host at a time.
func normalizeHost(input string) (string, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return "", errors.New("host is required")
	}
	if !strings.Contains(in, "://") {
		in = "http://" + in
	}
	u, err := url.Parse(in)
	if err != nil {
		return "", fmt.Errorf("invalid host %q: %v", input, err)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case host == "" || u.User != nil:
		return "", fmt.Errorf("invalid host %q", input)
	case strings.Contains(host, "*"):
		return "", fmt.Errorf("invalid host %q: wildcards cannot be requested; ask for one exact host", input)
	case net.ParseIP(host) == nil && strings.ContainsAny(host, " _/\\@"):
		return "", fmt.Errorf("invalid host %q", input)
	}
	return host, nil
}

// allowHost adds host to the session's allowed set, including the in-browser
// document filter, which reads the same set.
func (s *Session) allowHost(host string) {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	if !hostAllowed(s.hosts, host) {
		s.hosts = append(slices.Clip(s.hosts), host)
	}
}

func (s *Session) recordBlocked(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return
	}
	s.blockedMu.Lock()
	defer s.blockedMu.Unlock()
	if !slices.Contains(s.blocked, u.Hostname()) {
		s.blocked = append(s.blocked, u.Hostname())
	}
}

// takeBlocked returns and clears the hosts blocked since the last call.
func (s *Session) takeBlocked() []string {
	s.blockedMu.Lock()
	defer s.blockedMu.Unlock()
	out := s.blocked
	s.blocked = nil
	return out
}

// blockedNote explains filtered navigations to the model, or returns "".
func (s *Session) blockedNote(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hosts {
		fmt.Fprintf(&b, "navigation blocked: %v\n", s.notAllowedError(h))
	}
	return b.String()
}
