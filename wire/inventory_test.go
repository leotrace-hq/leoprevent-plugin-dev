package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// The snapshot is developer-authored config relayed over the wire, so both ends bound it. A
// snapshot with nothing in it is not sent at all.
func TestValidInventoryBoundsAndDropsEmpties(t *testing.T) {
	if ValidInventory(nil) != nil || ValidInventory(&AgentInventory{}) != nil {
		t.Fatal("an absent or empty snapshot must validate to nil")
	}
	if got := ValidInventory(&AgentInventory{AccountType: "enterprise-ultra"}); got != nil {
		t.Errorf("an account type outside the vocabulary must be dropped, got %+v", got)
	}

	no := false
	in := &AgentInventory{AccountType: AccountCompany, ManagedPolicy: &no}
	for i := 0; i < maxInventoryItems+50; i++ {
		in.Plugins = append(in.Plugins, InventoryPlugin{Name: "p"})
	}
	in.MCPServers = []InventoryMCPServer{
		{Name: "  playwright ", Package: strings.Repeat("x", maxInventoryText+40)},
		{Name: ""},                    // nameless: dropped
		{Name: "bad\nname"},           // control character: the name is dropped, so the item is
		{Name: "ok", Host: "h\x00st"}, // a bad optional field is blanked, the item is kept
	}
	got := ValidInventory(in)
	if got == nil {
		t.Fatal("a snapshot with content must survive")
	}
	if len(got.Plugins) != maxInventoryItems {
		t.Errorf("plugins = %d, want capped at %d", len(got.Plugins), maxInventoryItems)
	}
	if len(got.MCPServers) != 2 {
		t.Fatalf("mcp servers = %+v, want the two named ones", got.MCPServers)
	}
	if s := got.MCPServers[0]; s.Name != "playwright" || len(s.Package) != maxInventoryText {
		t.Errorf("first server = %+v, want a trimmed name and a capped package", s)
	}
	if s := got.MCPServers[1]; s.Name != "ok" || s.Host != "" {
		t.Errorf("second server = %+v, want its bad host blanked", s)
	}
	// "No managed policy" is a fact and must reach the wire, unlike an adapter that cannot tell.
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"managed_policy":false`) {
		t.Errorf("managed_policy=false must be sent, got %s", raw)
	}
}

// A connector's sign-in mark is one of two words. Anything else is dropped and the connector kept.
func TestValidInventoryKeepsOnlyKnownConnectorAuth(t *testing.T) {
	got := ValidInventory(&AgentInventory{Connectors: []InventoryConnector{
		{Name: "Linear", Auth: ConnectorSignedIn},
		{Name: "Notion", Auth: ConnectorNotSignedIn},
		{Name: "Slack", Auth: "token=abc"},
		{Name: "Asana"},
	}})
	if got == nil || len(got.Connectors) != 4 {
		t.Fatalf("connectors = %+v, want all four kept", got)
	}
	for i, want := range []string{ConnectorSignedIn, ConnectorNotSignedIn, "", ""} {
		if got.Connectors[i].Auth != want {
			t.Errorf("%s auth = %q, want %q", got.Connectors[i].Name, got.Connectors[i].Auth, want)
		}
	}
	// "Not signed in" is a fact and must reach the wire; "the session does not say" sends nothing.
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"auth":"not_signed_in"`) || strings.Count(string(raw), `"auth"`) != 2 {
		t.Errorf("wire form = %s", raw)
	}
}

// The instruction files and auto-started programs are bounded like every other list, a program
// of a kind outside the vocabulary is dropped, and "no rule allows every command" reaches the wire.
func TestValidInventoryBoundsInstructionsAndPrograms(t *testing.T) {
	no := false
	in := &AgentInventory{AllowsAnyCommand: &no}
	for i := 0; i < maxInventoryItems+5; i++ {
		in.Skills = append(in.Skills, InventoryInstruction{Name: "s"})
	}
	in.Commands = []InventoryInstruction{{Name: " deploy ", Source: ScopeProject, Hash: "0123456789abcdef"}, {Name: "bad\nname"}}
	in.Subagents = []InventoryInstruction{{Name: "reviewer", Source: ScopePlugin, Plugin: "kit@acme", Hash: "h\x00sh"}}
	in.Programs = []InventoryProgram{
		{Kind: ProgramStatusLine, Source: ScopeUser, Program: "status.sh", Hash: "0123456789abcdef"},
		{Kind: ProgramAPIKeyHelper, Program: "get\tkey"},
		{Kind: "cron_job", Program: "x"},
	}
	got := ValidInventory(in)
	if got == nil {
		t.Fatal("a snapshot holding only the permission flag must survive")
	}
	if len(got.Skills) != maxInventoryItems {
		t.Errorf("skills = %d, want capped at %d", len(got.Skills), maxInventoryItems)
	}
	if len(got.Commands) != 1 || got.Commands[0].Name != "deploy" {
		t.Errorf("commands = %+v, want the one cleanly named command, trimmed", got.Commands)
	}
	if len(got.Subagents) != 1 || got.Subagents[0].Plugin != "kit@acme" || got.Subagents[0].Hash != "" {
		t.Errorf("subagents = %+v, want it kept with its bad hash blanked", got.Subagents)
	}
	if len(got.Programs) != 2 || got.Programs[0].Program != "status.sh" || got.Programs[1].Program != "" {
		t.Errorf("programs = %+v, want the two known kinds, the second without its bad name", got.Programs)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"allows_any_command":false`) {
		t.Errorf("allows_any_command=false must be sent, got %s", raw)
	}
	if ValidInventory(&AgentInventory{AccountType: AccountAPIKey}) == nil {
		t.Error("the api_key account type is part of the vocabulary")
	}
}

func TestValidServersUsedAndText(t *testing.T) {
	var many []string
	for i := 0; i < maxServersUsed+10; i++ {
		many = append(many, "s")
	}
	if got := ValidServersUsed(many); len(got) != maxServersUsed {
		t.Errorf("servers used = %d, want capped at %d", len(got), maxServersUsed)
	}
	if got := ValidServersUsed([]string{"", " a ", "b\tc"}); len(got) != 1 || got[0] != "a" {
		t.Errorf("servers used = %q, want only the clean name", got)
	}
	if ValidInventoryText(" 2.1.317 ") != "2.1.317" || ValidInventoryText("a\x1bb") != "" {
		t.Error("a single value is trimmed, and dropped when it holds a control character")
	}
	// A cut never lands inside a multi-byte character.
	long := strings.Repeat("é", maxInventoryText)
	if got := ValidInventoryText(long); len(got) > maxInventoryText || !strings.HasPrefix(long, got) || got[len(got)-1] == 0xC3 {
		t.Errorf("cut split a character: %d bytes", len(got))
	}
}

func TestValidInstallID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	if got := ValidInstallID(id); got != id {
		t.Errorf("a well-formed id was changed: %q", got)
	}
	for name, bad := range map[string]string{
		"empty":      "",
		"short":      id[:31],
		"long":       id + "0",
		"upper case": strings.ToUpper(id),
		"not hex":    "g" + id[1:],
		"a hostname": "Florians-MacBook-Pro.local-00000",
		"padded":     " " + id[1:],
	} {
		if got := ValidInstallID(bad); got != "" {
			t.Errorf("%s: %q was accepted as %q", name, bad, got)
		}
	}
}
