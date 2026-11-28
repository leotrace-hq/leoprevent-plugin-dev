package transcript

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// mcpToolPrefix starts every MCP tool name: mcp__<server>__<tool>.
const mcpToolPrefix = "mcp__"

// MCPServerOf returns the server part of an MCP tool name ("mcp__playwright__browser_click" →
// "playwright"), or "" for a tool that is not an MCP tool. The server is everything up to the
// LAST double underscore: a server name may hold one itself, a tool name never starts with one.
func MCPServerOf(tool string) string {
	rest, ok := strings.CutPrefix(tool, mcpToolPrefix)
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, "__")
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

// SessionServers are the MCP servers a Claude Code session LOADED, used or not (LEO-822).
type SessionServers struct {
	// Names are the servers as tool names spell them, sorted ("claude_ai_Linear", "playwright",
	// or the desktop app's opaque connector id).
	Names []string
	// Display maps such a name to the readable name the session printed for it, where it printed
	// one ("claude_ai_Linear" → "claude.ai Linear").
	Display map[string]string
	// Tools maps such a name to the tools the session loaded or called on it, sorted. A server
	// known only by its instructions has none.
	Tools map[string][]string
}

// ParseSessionServers reads which MCP servers a Claude Code session loaded.
//
// Claude Code writes the tools it loaded into the transcript as attachments: the tool names
// (deferred_tools_delta) and each server's own instructions under its readable name
// (mcp_instructions_delta). Tool calls add any server neither attachment named. Together they are
// every server the session shows, without opening a config file.
//
// Best-effort: a session that defers no tools and calls none of a server's reports that server
// only if it sent instructions. An unreadable transcript is an error; an unparseable line is
// skipped.
func ParseSessionServers(transcriptPath string) (SessionServers, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return SessionServers{}, err
	}
	defer f.Close()
	lines, err := readJSONLLines(f)
	if err != nil {
		return SessionServers{}, err
	}
	loaded := map[string]bool{}
	display := map[string]string{}
	tools := map[string]map[string]bool{}
	sawTool := func(name string) {
		s := MCPServerOf(name)
		if s == "" {
			return
		}
		loaded[s] = true
		if tools[s] == nil {
			tools[s] = map[string]bool{}
		}
		tools[s][name[len(mcpToolPrefix)+len(s)+len("__"):]] = true
	}
	for _, line := range lines {
		var e struct {
			Type       string `json:"type"`
			Attachment struct {
				Type         string   `json:"type"`
				AddedNames   []string `json:"addedNames"`
				RemovedNames []string `json:"removedNames"`
			} `json:"attachment"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch {
		case e.Type == "attachment" && e.Attachment.Type == "deferred_tools_delta":
			for _, tool := range e.Attachment.AddedNames {
				sawTool(tool)
			}
		case e.Type == "attachment" && e.Attachment.Type == "mcp_instructions_delta":
			for _, name := range e.Attachment.AddedNames {
				s := NormalizeServerName(name)
				loaded[s] = true
				if s != name {
					display[s] = name
				}
			}
			for _, name := range e.Attachment.RemovedNames {
				delete(loaded, NormalizeServerName(name))
			}
		case e.Type == "assistant":
			var blocks []contentBlock
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "tool_use" {
					sawTool(b.Name)
				}
			}
		}
	}
	out := SessionServers{Display: display, Tools: map[string][]string{}}
	for s := range loaded {
		out.Names = append(out.Names, s)
		for tool := range tools[s] {
			out.Tools[s] = append(out.Tools[s], tool)
		}
		sort.Strings(out.Tools[s])
	}
	sort.Strings(out.Names)
	return out, nil
}

// NormalizeServerName spells a server's readable name the way Claude Code spells it inside a tool
// name: every character outside [A-Za-z0-9_-] becomes an underscore ("claude.ai Linear" →
// "claude_ai_Linear").
func NormalizeServerName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, name)
}
