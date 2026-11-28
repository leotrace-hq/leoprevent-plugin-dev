package transcript

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMCPServerOf(t *testing.T) {
	for tool, want := range map[string]string{
		"mcp__playwright__browser_click":                           "playwright",
		"mcp__claude_ai_Linear__get_issue":                         "claude_ai_Linear",
		"mcp__258c2c3c-03ce-4c9e-9761-69e34ce83b4d__list_comments": "258c2c3c-03ce-4c9e-9761-69e34ce83b4d",
		"mcp__plugin_acme__tools__run":                             "plugin_acme__tools", // a server name may hold a double underscore
		"Edit":                                                     "",
		"mcp__":                                                    "",
		"mcp__server":                                              "",
		"mcp____tool":                                              "",
		"mcp__ccd_session__mark_chapter":                           "ccd_session",
		"mcp__Claude_Code_iOS_Simulator__control":                  "Claude_Code_iOS_Simulator",
		"mcp__08b2f74e-3b2a-42be-be16-3db0e463ac41__slack_send_message": "08b2f74e-3b2a-42be-be16-3db0e463ac41",
	} {
		if got := MCPServerOf(tool); got != want {
			t.Errorf("MCPServerOf(%q) = %q, want %q", tool, got, want)
		}
	}
}

// The agent's version and approval mode are stamped on the transcript's own lines, and the MCP
// servers a turn used are the servers of its tool calls: all three come out of the parse the
// turn meta already does, with no second read.
func TestParseTurnMetaReadsTheInventoryFields(t *testing.T) {
	tp := writeLines(t,
		`{"type":"user","version":"2.1.300","permissionMode":"default","message":{"role":"user","content":"earlier turn"}}`,
		`{"type":"assistant","version":"2.1.300","message":{"id":"m0","role":"assistant","content":[{"type":"tool_use","name":"mcp__old_server__x","input":{}},{"type":"tool_use","name":"Skill","input":{"skill":"earlier-skill"}}]}}`,
		`{"type":"user","version":"2.1.317","permissionMode":"auto","message":{"role":"user","content":"open the ticket"}}`,
		`{"type":"assistant","version":"2.1.317","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","name":"mcp__claude_ai_Linear__get_issue","input":{}},{"type":"tool_use","name":"Read","input":{}}]}}`,
		// The same message logged again, as Claude Code does: its tools are not counted twice.
		`{"type":"assistant","version":"2.1.317","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","name":"mcp__claude_ai_Linear__get_issue","input":{}}]}}`,
		`{"type":"assistant","version":"2.1.317","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","name":"mcp__playwright__browser_click","input":{}},{"type":"tool_use","name":"mcp__claude_ai_Linear__list_comments","input":{}}]}}`,
		// Skills the model loaded: one of the project's twice, one a plugin's, one call with no name.
		`{"type":"assistant","version":"2.1.317","message":{"id":"m3","role":"assistant","content":[{"type":"tool_use","name":"Skill","input":{"skill":"design-system"}},{"type":"tool_use","name":"Skill","input":{"skill":"leoprevent:stats","args":"last week"}},{"type":"tool_use","name":"Skill","input":{"skill":"design-system"}},{"type":"tool_use","name":"Skill","input":{}}]}}`,
	)
	m, err := ParseTurnMeta(tp)
	if err != nil {
		t.Fatal(err)
	}
	if m.AgentVersion != "2.1.317" || m.PermissionMode != "auto" {
		t.Errorf("version/mode = %q/%q, want the latest line's 2.1.317/auto", m.AgentVersion, m.PermissionMode)
	}
	// This turn's servers only, each once, in the order first used.
	if want := []string{"claude_ai_Linear", "playwright"}; !reflect.DeepEqual(m.MCPServersUsed, want) {
		t.Errorf("servers used = %q, want %q", m.MCPServersUsed, want)
	}
	// The same for skills: this turn's only, each once, by the name the model called it with.
	if want := []string{"design-system", "leoprevent:stats"}; !reflect.DeepEqual(m.SkillsUsed, want) {
		t.Errorf("skills used = %q, want %q", m.SkillsUsed, want)
	}
}

// A skill's text is written into the transcript as a user entry in the middle of the turn. It is
// not a prompt: the turn still starts at what the developer typed, so the Skill call that came
// before the text, and everything else before it, stays part of the turn.
func TestALoadedSkillsTextDoesNotStartANewTurn(t *testing.T) {
	tp := writeLines(t,
		`{"type":"user","timestamp":"2026-11-13T14:00:00Z","message":{"role":"user","content":"restyle the panel"}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","name":"mcp__claude_ai_Linear__get_issue","input":{}},{"type":"tool_use","id":"toolu_1","name":"Skill","input":{"skill":"design-system"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Launching skill: design-system"}]}}`,
		`{"type":"user","isMeta":true,"sourceToolUseID":"toolu_1","timestamp":"2026-11-13T14:00:05Z","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /home/ada/code/shop/.claude/skills/design-system\n\n# The design system"}]}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/home/ada/code/shop/panel.css","old_string":"a","new_string":"b"}}]}}`,
	)
	m, err := ParseTurnMeta(tp)
	if err != nil {
		t.Fatal(err)
	}
	if m.Prompt != "restyle the panel" {
		t.Errorf("prompt = %q, want what the developer typed, not the skill's text", m.Prompt)
	}
	if want := []string{"design-system"}; !reflect.DeepEqual(m.SkillsUsed, want) {
		t.Errorf("skills used = %q, want %q", m.SkillsUsed, want)
	}
	if want := []string{"claude_ai_Linear"}; !reflect.DeepEqual(m.MCPServersUsed, want) {
		t.Errorf("servers used = %q, want the server called before the skill loaded", m.MCPServersUsed)
	}
}

// A transcript from before these fields existed, or from a turn that called no MCP tool, yields
// none of them, and nothing else about the parse changes.
func TestParseTurnMetaWithoutInventoryFields(t *testing.T) {
	tp := writeLines(t,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"hello"}]}}`,
	)
	m, err := ParseTurnMeta(tp)
	if err != nil {
		t.Fatal(err)
	}
	if m.AgentVersion != "" || m.PermissionMode != "" || m.MCPServersUsed != nil || m.SkillsUsed != nil {
		t.Errorf("inventory fields must stay empty, got %+v", m)
	}
	if m.AgentModel != "claude-opus-4-8" || m.Prompt != "hi" {
		t.Errorf("the rest of the parse changed: %+v", m)
	}
}

// Every server the session loaded is found, used or not: from the deferred tool list, from the
// servers' instructions (which carry the readable name), and from tool calls.
func TestParseSessionServers(t *testing.T) {
	tp := writeLines(t,
		`{"type":"attachment","attachment":{"type":"deferred_tools_delta","addedNames":["WebFetch","mcp__claude_ai_Linear__get_issue","mcp__claude_ai_Linear__list_comments","mcp__playwright__browser_click","mcp__258c2c3c-03ce-4c9e-9761-69e34ce83b4d__get_issue"]}}`,
		`{"type":"attachment","attachment":{"type":"mcp_instructions_delta","addedNames":["claude.ai Linear","claude.ai Claude Docs","gone"],"removedNames":[]}}`,
		`{"type":"attachment","attachment":{"type":"mcp_instructions_delta","addedNames":[],"removedNames":["gone"]}}`,
		`not json at all`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"mcp__visualize__show_widget","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":"a plain string content is not a block list"}}`,
	)
	got, err := ParseSessionServers(tp)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"258c2c3c-03ce-4c9e-9761-69e34ce83b4d", "claude_ai_Claude_Docs", "claude_ai_Linear", "playwright", "visualize"}
	if !reflect.DeepEqual(got.Names, want) {
		t.Errorf("names = %q, want %q", got.Names, want)
	}
	if got.Display["claude_ai_Linear"] != "claude.ai Linear" || got.Display["claude_ai_Claude_Docs"] != "claude.ai Claude Docs" {
		t.Errorf("display names = %v, want the readable names the session printed", got.Display)
	}
	wantTools := map[string][]string{
		"258c2c3c-03ce-4c9e-9761-69e34ce83b4d": {"get_issue"},
		"claude_ai_Linear":                     {"get_issue", "list_comments"},
		"playwright":                           {"browser_click"},
		"visualize":                            {"show_widget"},
	}
	if !reflect.DeepEqual(got.Tools, wantTools) {
		t.Errorf("tools = %v, want %v (a server named only by its instructions has none)", got.Tools, wantTools)
	}
	if _, err := ParseSessionServers(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("an unreadable transcript must be an error, not an empty session")
	}
}
