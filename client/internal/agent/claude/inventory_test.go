package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// write creates path under dir with content, making parents.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// install lays out a Claude Code home, a project and (optionally) the desktop app's session
// files under a temp dir, points the collector at them, and returns the Stop event for a session
// in that project. Every value named SECRET-… below must never reach the snapshot.
type install struct {
	home, cfg, project, appData string
}

func newInstall(t *testing.T) install {
	t.Helper()
	root := t.TempDir()
	in := install{
		home:    filepath.Join(root, "home", "ada"),
		project: filepath.Join(root, "home", "ada", "code", "shop"),
		appData: filepath.Join(root, "appdata"),
	}
	in.cfg = filepath.Join(in.home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	// A key in the developer's own shell must not turn every test's account into "api_key".
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	oldHome, oldCfg, oldManaged := userHomeDir, userConfigDir, managedSettingsFiles
	userHomeDir = func() (string, error) { return in.home, nil }
	userConfigDir = func() (string, error) { return in.appData, nil }
	managedSettingsFiles = func() []string { return []string{filepath.Join(root, "managed", "managed-settings.json")} }
	t.Cleanup(func() { userHomeDir, userConfigDir, managedSettingsFiles = oldHome, oldCfg, oldManaged })
	write(t, filepath.Join(in.project, ".git", "HEAD"), "ref: refs/heads/main\n")
	return in
}

func (in install) event(t *testing.T, transcriptLines ...string) agent.Event {
	t.Helper()
	tp := filepath.Join(in.cfg, "projects", "shop", "session.jsonl")
	write(t, tp, strings.Join(transcriptLines, "\n")+"\n")
	return agent.Event{SessionID: "sess-1", Cwd: in.project, TranscriptPath: tp}
}

const secretMarker = "SECRET-"

// The whole point of the collector's narrow structs: a realistic install full of credentials
// yields a snapshot with the expected names and versions and NOT ONE secret, path or account
// detail. The assertion is on the marshalled bytes, so a field added later is covered too.
func TestInventoryCarriesMetadataAndNoSecrets(t *testing.T) {
	in := newInstall(t)

	write(t, filepath.Join(in.home, ".claude.json"), `{
	  "oauthAccount": {"organizationType": "claude_team", "emailAddress": "SECRET-ada@acme.example",
	    "organizationName": "SECRET-Acme Corp", "accountUuid": "SECRET-uuid", "billingType": "SECRET-stripe"},
	  "claudeAiMcpEverConnected": ["claude.ai Linear", "claude.ai Google Drive"],
	  "mcpServers": {
	    "playwright": {"type": "stdio", "command": "npx", "args": ["@playwright/mcp@latest", "--browser", "SECRET-arg"]},
	    "github": {"command": "docker", "args": ["run", "-i", "--rm", "-e", "GITHUB_TOKEN=SECRET-ghp", "ghcr.io/github/github-mcp-server", "SECRET-after-image"],
	               "env": {"GITHUB_TOKEN": "SECRET-env-token"}},
	    "sentry": {"type": "http", "url": "https://SECRET-user:SECRET-pass@mcp.sentry.dev/mcp?token=SECRET-query",
	               "headers": {"Authorization": "Bearer SECRET-header"}}
	  },
	  "projects": {
	    "`+filepath.ToSlash(in.project)+`": {"mcpServers": {"db": {"command": "/opt/SECRET-path/bin/dbmcp", "args": ["--dsn", "postgres://SECRET-dsn"]}}},
	    "/elsewhere/SECRET-other-project": {"mcpServers": {"other": {"command": "npx", "args": ["other-pkg"]}}}
	  }
	}`)
	write(t, filepath.Join(in.project, ".mcp.json"), `{"mcpServers": {
	  "docs": {"command": "uvx", "args": ["--from", "docs-mcp==1.4.0", "docs-server", "--key", "SECRET-uvx-key"]}}}`)

	write(t, filepath.Join(in.cfg, "settings.json"), `{
	  "enabledPlugins": {"leoprevent@leotrace": true, "linter@acme-tools": true, "old@acme-tools": false},
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "API_KEY=SECRET-inline `+in.home+`/bin/notify.sh --token SECRET-hook-arg"}]}]},
	  "statusLine": {"type": "command", "command": "TOKEN=SECRET-status-env `+in.home+`/bin/status.sh --key SECRET-status-arg", "padding": 0},
	  "permissions": {"allow": ["Bash(npm run SECRET-rule:*)", "Bash", "Read(/srv/SECRET-allowed/**)"], "deny": ["Read(/srv/SECRET-denied/**)"]}
	}`)
	// Instruction files: their text is hashed and must not leave, their names do.
	skill := func(body string) string {
		return "---\nname: SECRET-frontmatter-name\ndescription: SECRET-description\n---\n" + body
	}
	write(t, filepath.Join(in.cfg, "skills", "release-notes", "SKILL.md"), skill("SECRET-user skill text"))
	write(t, filepath.Join(in.cfg, "skills", ".trash", "gone", "SKILL.md"), skill("SECRET-trashed"))
	write(t, filepath.Join(in.cfg, "skills", "bad $(name)", "SKILL.md"), skill("SECRET-badly named"))
	write(t, filepath.Join(in.cfg, "skills", "synced", "SECRET-account_SECRET-org", "pdf", "SKILL.md"), skill("SECRET-account-synced skill text"))
	write(t, filepath.Join(in.project, ".claude", "skills", "deploy", "SKILL.md"), skill("SECRET-project skill text"))
	write(t, filepath.Join(in.project, ".claude", "commands", "db", "migrate.md"), "SECRET-command text")
	write(t, filepath.Join(in.cfg, "agents", "review", "security.md"), "---\nname: sec-reviewer\ndescription: SECRET-agent description\n---\nSECRET-agent prompt")
	write(t, filepath.Join(in.project, ".claude", "settings.json"), `{
	  "enabledPlugins": {"linter@acme-tools": false},
	  "hooks": {"SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "./scripts/setup.sh"}]}],
	            "PreToolUse": [{"hooks": [{"type": "prompt", "prompt": "SECRET-prompt text"}]}]}
	}`)

	pluginDir := filepath.Join(in.cfg, "plugins", "cache", "leotrace", "leoprevent", "0.2.48")
	write(t, filepath.Join(in.cfg, "plugins", "installed_plugins.json"), `{"version": 2, "plugins": {
	  "leoprevent@leotrace": [{"scope": "user", "version": "0.2.48", "gitCommitSha": "6feaf028", "installPath": "`+filepath.ToSlash(pluginDir)+`"}],
	  "linter@acme-tools": [{"scope": "user", "version": "1.0.0", "installPath": "`+filepath.ToSlash(filepath.Join(in.cfg, "plugins", "cache", "linter"))+`"}],
	  "old@acme-tools": {"scope": "user", "version": "0.1.0"},
	  "elsewhere@acme-tools": [{"scope": "project", "version": "9.9.9", "projectPath": "/elsewhere/SECRET-other-project"}]
	}}`)
	write(t, filepath.Join(pluginDir, ".claude-plugin", "plugin.json"), `{"name": "leoprevent", "hooks": "./hooks/hooks.claude.json"}`)
	write(t, filepath.Join(pluginDir, "hooks", "hooks.claude.json"), `{"hooks": {
	  "Stop": [{"hooks": [{"type": "command", "command": "\"${CLAUDE_PLUGIN_ROOT}/bin/leoprevent-plugin\" --agent=claude"}]}]}}`)
	write(t, filepath.Join(pluginDir, "commands", "stats.md"), "SECRET-plugin command text")
	write(t, filepath.Join(pluginDir, "skills", "scan", "SKILL.md"), skill("SECRET-plugin skill text"))
	write(t, filepath.Join(pluginDir, "agents", "review", "audit.md"), "SECRET-plugin agent prompt, no frontmatter")
	// A disabled plugin's instructions are not read, like its hooks.
	write(t, filepath.Join(in.cfg, "plugins", "cache", "linter", "skills", "lint", "SKILL.md"), skill("SECRET-disabled plugin"))

	ev := in.event(t,
		`{"type":"attachment","attachment":{"type":"deferred_tools_delta","addedNames":["mcp__claude_ai_Linear__get_issue","mcp__claude_ai_Google_Drive__search_files","mcp__playwright__browser_click"]}}`,
		`{"type":"attachment","attachment":{"type":"mcp_instructions_delta","addedNames":["claude.ai Linear"]}}`,
	)

	got, err := New().Inventory(ev)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), secretMarker) {
		t.Fatalf("a secret reached the snapshot:\n%s", raw)
	}
	for _, leak := range []string{in.home, "ada", "/opt/", "elsewhere", "GITHUB_TOKEN", "--browser", "--dsn", "setup.sh --", "@acme.example"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("snapshot holds %q, which must stay on the machine:\n%s", leak, raw)
		}
	}

	servers := map[string]wire.InventoryMCPServer{}
	for _, s := range got.MCPServers {
		servers[s.Name] = s
	}
	wantServers := map[string]wire.InventoryMCPServer{
		"playwright": {Name: "playwright", Scope: wire.ScopeUser, Transport: "stdio", Launcher: "npx", Package: "@playwright/mcp@latest"},
		"github":     {Name: "github", Scope: wire.ScopeUser, Transport: "stdio", Launcher: "docker", Package: "ghcr.io/github/github-mcp-server"},
		"sentry":     {Name: "sentry", Scope: wire.ScopeUser, Transport: "http", Host: "mcp.sentry.dev"},
		"db":         {Name: "db", Scope: wire.ScopeLocal, Transport: "stdio", Launcher: "dbmcp"},
		"docs":       {Name: "docs", Scope: wire.ScopeProject, Transport: "stdio", Launcher: "uvx", Package: "docs-mcp==1.4.0"},
	}
	if len(servers) != len(wantServers) {
		t.Errorf("mcp servers = %+v, want exactly %d (another project's are not this one's)", got.MCPServers, len(wantServers))
	}
	for name, want := range wantServers {
		if servers[name] != want {
			t.Errorf("server %s = %+v, want %+v", name, servers[name], want)
		}
	}

	plugins := map[string]wire.InventoryPlugin{}
	for _, p := range got.Plugins {
		plugins[p.Name] = p
	}
	if p := plugins["leoprevent"]; p != (wire.InventoryPlugin{Name: "leoprevent", Marketplace: "leotrace", Version: "0.2.48", Commit: "6feaf028", Scope: "user", Enabled: true}) {
		t.Errorf("leoprevent plugin = %+v", p)
	}
	// The project's settings switch the linter off: a later layer wins.
	if p := plugins["linter"]; p.Enabled || p.Version != "1.0.0" {
		t.Errorf("linter plugin = %+v, want installed but switched off by the project", p)
	}
	if p, ok := plugins["old"]; !ok || p.Enabled {
		t.Errorf("old plugin = %+v ok=%v, want listed and disabled (single-object install form)", p, ok)
	}
	if _, ok := plugins["elsewhere"]; ok {
		t.Error("a plugin installed for another project must not be listed")
	}

	type hk struct{ event, source, plugin, program string }
	hooks := map[hk]string{}
	for _, h := range got.Hooks {
		if h.Hash == "" {
			t.Errorf("hook %+v has no hash", h)
		}
		hooks[hk{h.Event, h.Source, h.Plugin, h.Program}] = h.Hash
	}
	for _, want := range []hk{
		{"Stop", wire.ScopeUser, "", "notify.sh"},
		{"SessionStart", wire.ScopeProject, "", "setup.sh"},
		{"PreToolUse", wire.ScopeProject, "", "prompt"},
		{"Stop", wire.ScopePlugin, "leoprevent@leotrace", "leoprevent-plugin"},
	} {
		if _, ok := hooks[want]; !ok {
			t.Errorf("missing hook %+v in %+v", want, got.Hooks)
		}
	}
	if len(got.Hooks) != 4 {
		t.Errorf("hooks = %+v, want 4 (a disabled plugin's hooks are not read)", got.Hooks)
	}

	connectors := map[string]wire.InventoryConnector{}
	for _, c := range got.Connectors {
		connectors[c.Name] = c
	}
	// One name from the session's own instructions, one spelled back from the install's list.
	if c := connectors["Linear"]; c.ID != "claude_ai_Linear" || c.Host != "" || c.Auth != wire.ConnectorSignedIn {
		t.Errorf("Linear connector = %+v", c)
	}
	if c := connectors["Google Drive"]; c.ID != "claude_ai_Google_Drive" {
		t.Errorf("Google Drive connector = %+v (all: %+v)", c, got.Connectors)
	}
	if len(got.Connectors) != 2 {
		t.Errorf("connectors = %+v, want the two claude.ai ones; a local MCP server is not a connector", got.Connectors)
	}

	type ins struct{ name, source, plugin string }
	listed := func(items []wire.InventoryInstruction) map[ins]bool {
		out := map[ins]bool{}
		for _, i := range items {
			if len(i.Hash) != 16 {
				t.Errorf("instruction %+v has no 16-hex hash", i)
			}
			out[ins{i.Name, i.Source, i.Plugin}] = true
		}
		return out
	}
	// A skill is named by its folder, whatever its frontmatter says. The trash, the badly named
	// one and the disabled plugin's are left out; the account's comes from the synced folder.
	wantSkills := map[ins]bool{
		{"release-notes", wire.ScopeUser, ""}: true, {"pdf", wire.ScopeAccount, ""}: true,
		{"deploy", wire.ScopeProject, ""}: true, {"scan", wire.ScopePlugin, "leoprevent@leotrace"}: true,
	}
	if s := listed(got.Skills); !reflect.DeepEqual(s, wantSkills) {
		t.Errorf("skills = %v, want %v", s, wantSkills)
	}
	wantCommands := map[ins]bool{{"db:migrate", wire.ScopeProject, ""}: true, {"stats", wire.ScopePlugin, "leoprevent@leotrace"}: true}
	if c := listed(got.Commands); !reflect.DeepEqual(c, wantCommands) {
		t.Errorf("commands = %v, want %v", c, wantCommands)
	}
	// A subagent is named by its `name:` line; a plugin's keeps its subfolder.
	wantAgents := map[ins]bool{{"sec-reviewer", wire.ScopeUser, ""}: true, {"review:audit", wire.ScopePlugin, "leoprevent@leotrace"}: true}
	if a := listed(got.Subagents); !reflect.DeepEqual(a, wantAgents) {
		t.Errorf("subagents = %v, want %v", a, wantAgents)
	}
	if len(got.Programs) != 1 || got.Programs[0].Kind != wire.ProgramStatusLine || got.Programs[0].Source != wire.ScopeUser ||
		got.Programs[0].Program != "status.sh" || len(got.Programs[0].Hash) != 16 {
		t.Errorf("programs = %+v, want the user's status line as status.sh with a hash", got.Programs)
	}
	if got.AllowsAnyCommand == nil || !*got.AllowsAnyCommand {
		t.Errorf("allows any command = %v, want true: the user's settings allow all of Bash", got.AllowsAnyCommand)
	}

	if got.AccountType != wire.AccountCompany {
		t.Errorf("account type = %q, want company", got.AccountType)
	}
	if got.ManagedPolicy == nil || *got.ManagedPolicy {
		t.Errorf("managed policy = %v, want an explicit false", got.ManagedPolicy)
	}
}

// One hook must hash the same on every machine, so the home directory is taken out before
// hashing: that is what lets a dashboard say "this hook runs on forty machines".
func TestHookHashIsStableAcrossHomes(t *testing.T) {
	a := hook("Stop", wire.ScopeUser, "", hookEntry{Type: "command", Command: "/Users/ada/bin/notify.sh --quiet"}, "/Users/ada")
	b := hook("Stop", wire.ScopeUser, "", hookEntry{Type: "command", Command: "/home/bob/bin/notify.sh --quiet"}, "/home/bob")
	c := hook("Stop", wire.ScopeUser, "", hookEntry{Type: "command", Command: "/home/bob/bin/notify.sh --loud"}, "/home/bob")
	if a.Hash != b.Hash || a.Program != "notify.sh" {
		t.Errorf("same hook, different homes: %+v vs %+v", a, b)
	}
	if a.Hash == c.Hash {
		t.Error("a different command must hash differently")
	}
}

// The desktop app names a connector by an opaque id in the transcript. Its own session files
// name the id, and are the only place the host is found.
func TestInventoryResolvesDesktopConnectorIDs(t *testing.T) {
	in := newInstall(t)
	const linear, unknown = "258c2c3c-03ce-4c9e-9761-69e34ce83b4d", "11111111-2222-3333-4444-555555555555"
	sessions := filepath.Join(in.appData, "Claude", "claude-code-sessions", "acct", "org")
	write(t, filepath.Join(sessions, "local_a.json"), `{"cliSessionId": "other", "remoteMcpServersConfig": [
	  {"uuid": "`+linear+`", "name": "Linear", "url": "https://mcp.linear.app/mcp?SECRET-q=1", "tools": [{"name": "get_issue"}]}]}`)
	write(t, filepath.Join(sessions, "local_b.json"), `not json`)

	ev := in.event(t,
		`{"type":"attachment","attachment":{"type":"deferred_tools_delta","addedNames":["mcp__`+linear+`__get_issue","mcp__`+unknown+`__x","mcp__ccd_session__mark_chapter"]}}`)
	got, err := New().Inventory(ev)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(got.Connectors) != 1 || got.Connectors[0] != (wire.InventoryConnector{Name: "Linear", ID: linear, Host: "mcp.linear.app", Auth: wire.ConnectorSignedIn}) {
		t.Errorf("connectors = %+v, want Linear resolved with its host and the unnamed id left out", got.Connectors)
	}
	// No signed-in account in this install's config: the flag is absent, not "unknown".
	if got.AccountType != "" {
		t.Errorf("account type = %q, want empty", got.AccountType)
	}
}

// The terminal lists every connector on offer, signed in to or not. One the developer has not
// signed in to loads only the two tools that start the sign-in, and that is how it is told apart:
// the agent holds no access through it.
func TestInventoryMarksConnectorsNotSignedIn(t *testing.T) {
	in := newInstall(t)
	ev := in.event(t,
		`{"type":"attachment","attachment":{"type":"deferred_tools_delta","addedNames":[`+
			`"mcp__claude_ai_Notion__authenticate","mcp__claude_ai_Notion__complete_authentication",`+
			`"mcp__claude_ai_Slack__authenticate","mcp__claude_ai_Slack__complete_authentication",`+
			`"mcp__claude_ai_Linear__get_issue"]}}`,
		`{"type":"attachment","attachment":{"type":"mcp_instructions_delta","addedNames":["claude.ai Asana"]}}`,
		// Signed in to part-way through the session: its real tools arrive later.
		`{"type":"attachment","attachment":{"type":"deferred_tools_delta","addedNames":["mcp__claude_ai_Slack__slack_read_channel"],"removedNames":["mcp__claude_ai_Slack__authenticate"]}}`)
	got, err := New().Inventory(ev)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	auth := map[string]string{}
	for _, c := range got.Connectors {
		auth[c.Name] = c.Auth
	}
	want := map[string]string{
		"Notion": wire.ConnectorNotSignedIn,
		"Slack":  wire.ConnectorSignedIn,
		"Linear": wire.ConnectorSignedIn,
		"Asana":  "", // named by its instructions only: the session does not say
	}
	if !reflect.DeepEqual(auth, want) {
		t.Errorf("connector sign-in = %v, want %v", auth, want)
	}
}

func TestAccountType(t *testing.T) {
	for org, want := range map[string]string{
		"claude_team": wire.AccountCompany, "claude_enterprise": wire.AccountCompany,
		"claude_pro": wire.AccountPersonal, "claude_max": wire.AccountPersonal,
		"something_new": wire.AccountUnknown, "": wire.AccountUnknown,
	} {
		var g globalConfig
		_ = json.Unmarshal([]byte(`{"oauthAccount":{"organizationType":"`+org+`"}}`), &g)
		if got := accountType(g, nil, noEnv); got != want {
			t.Errorf("accountType(%q) = %q, want %q", org, got, want)
		}
	}
	if got := accountType(globalConfig{}, nil, noEnv); got != "" {
		t.Errorf("no account = %q, want empty", got)
	}
}

func noEnv(string) string { return "" }

// A key outranks a signed-in account in Claude Code's own order, so an install that has both
// reports the key. Only whether one is set is looked at.
func TestAccountTypeIsAPIKeyWhenAKeyIsSet(t *testing.T) {
	var company globalConfig
	_ = json.Unmarshal([]byte(`{"oauthAccount":{"organizationType":"claude_team"}}`), &company)
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		env := func(k string) string {
			if k == name {
				return "SECRET-key"
			}
			return ""
		}
		if got := accountType(company, nil, env); got != wire.AccountAPIKey {
			t.Errorf("%s set: account type = %q, want api_key", name, got)
		}
	}
	helper := []settingsLayer{{scope: wire.ScopeUser, settings: settingsFile{APIKeyHelper: json.RawMessage(`"/opt/bin/get-key.sh"`)}}}
	if got := accountType(company, helper, noEnv); got != wire.AccountAPIKey {
		t.Errorf("apiKeyHelper set: account type = %q, want api_key", got)
	}
	blank := []settingsLayer{{settings: settingsFile{APIKeyHelper: json.RawMessage(`"  "`)}}}
	if got := accountType(company, blank, noEnv); got != wire.AccountCompany {
		t.Errorf("blank apiKeyHelper: account type = %q, want company", got)
	}
}

// The permission flag is one yes/no answer: some settings file allows all of Bash and none
// denies it. A narrower rule is not it, and a deny from any file beats an allow from any other.
func TestAllowsAnyCommand(t *testing.T) {
	layer := func(scope, permissions string) settingsLayer {
		return settingsLayer{scope: scope, settings: settingsFile{Permissions: json.RawMessage(permissions)}}
	}
	for name, tc := range map[string]struct {
		layers []settingsLayer
		want   bool
	}{
		"no settings":         {nil, false},
		"no permissions":      {[]settingsLayer{{scope: wire.ScopeUser}}, false},
		"bare Bash":           {[]settingsLayer{layer(wire.ScopeUser, `{"allow":["Read","Bash"]}`)}, true},
		"Bash(*)":             {[]settingsLayer{layer(wire.ScopeProject, `{"allow":[" Bash(*) "]}`)}, true},
		"a narrower rule":     {[]settingsLayer{layer(wire.ScopeUser, `{"allow":["Bash(git status:*)","Bash(npm run *)"]}`)}, false},
		"denied elsewhere":    {[]settingsLayer{layer(wire.ScopeUser, `{"allow":["Bash"]}`), layer(wire.ScopeManaged, `{"deny":["Bash"]}`)}, false},
		"a narrower deny":     {[]settingsLayer{layer(wire.ScopeUser, `{"allow":["Bash"],"deny":["Bash(rm -rf:*)"]}`)}, true},
		"an unexpected shape": {[]settingsLayer{layer(wire.ScopeUser, `"allow everything"`)}, false},
	} {
		if got := allowsAnyCommand(tc.layers); got == nil || *got != tc.want {
			t.Errorf("%s: allows any command = %v, want %v", name, got, tc.want)
		}
	}
}

// The API key helper is a program Claude Code starts by itself, so it is listed like a hook, and
// a setting of an unexpected shape costs only itself: the same file's hooks are still read.
func TestProgramsAndAnOddlyShapedSetting(t *testing.T) {
	in := newInstall(t)
	write(t, filepath.Join(in.cfg, "settings.json"), `{
	  "apiKeyHelper": "`+in.home+`/bin/get-key.sh --vault SECRET-vault",
	  "statusLine": "not an object",
	  "permissions": ["not", "an", "object"],
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "notify.sh"}]}]}
	}`)
	write(t, filepath.Join(in.project, ".claude", "settings.local.json"), `{"statusLine": {"type": "command", "command": "bash ~/.claude/line.sh"}}`)
	got, err := New().Inventory(in.event(t))
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	type prog struct{ kind, source, program string }
	have := map[prog]bool{}
	for _, p := range got.Programs {
		have[prog{p.Kind, p.Source, p.Program}] = true
	}
	want := map[prog]bool{
		{wire.ProgramAPIKeyHelper, wire.ScopeUser, "get-key.sh"}: true,
		{wire.ProgramStatusLine, wire.ScopeLocal, "bash"}:        true,
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("programs = %v, want %v", have, want)
	}
	if got.AccountType != wire.AccountAPIKey {
		t.Errorf("account type = %q, want api_key: a key helper is configured", got.AccountType)
	}
	if len(got.Hooks) != 1 || got.Hooks[0].Program != "notify.sh" {
		t.Errorf("hooks = %+v, want the Stop hook despite the oddly shaped settings beside it", got.Hooks)
	}
	if got.AllowsAnyCommand == nil || *got.AllowsAnyCommand {
		t.Errorf("allows any command = %v, want an explicit false", got.AllowsAnyCommand)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), secretMarker) {
		t.Errorf("a secret reached the snapshot: %s", raw)
	}
}

// A plugin's manifest can name its instruction files itself. For skills that adds to skills/;
// for commands and subagents it replaces the default folder. A path that leaves the plugin's
// directory is not followed.
func TestPluginManifestNamesItsInstructions(t *testing.T) {
	in := newInstall(t)
	dir := filepath.Join(in.cfg, "plugins", "cache", "acme", "kit", "1.0.0")
	write(t, filepath.Join(in.cfg, "settings.json"), `{"enabledPlugins": {"kit@acme": true}}`)
	write(t, filepath.Join(in.cfg, "plugins", "installed_plugins.json"), `{"plugins": {
	  "kit@acme": [{"scope": "user", "version": "1.0.0", "installPath": "`+filepath.ToSlash(dir)+`"}]}}`)
	write(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name": "kit",
	  "skills": ["./extra-skills", "./single", "../../../../outside-skills"],
	  "commands": {"ship": {"source": "./cmd/ship.md"}, "hello": {"content": "say hello"}, "escape": {"source": "../../../../outside.md"}},
	  "agents": ["./custom/reviewer.md", "../../../../outside-agent.md"]}`)
	write(t, filepath.Join(dir, "skills", "default-skill", "SKILL.md"), "x")
	write(t, filepath.Join(dir, "extra-skills", "extra", "SKILL.md"), "x")
	write(t, filepath.Join(dir, "single", "SKILL.md"), "---\nname: solo\n---\nx")
	write(t, filepath.Join(dir, "cmd", "ship.md"), "x")
	write(t, filepath.Join(dir, "commands", "ignored.md"), "the manifest replaces this folder")
	write(t, filepath.Join(dir, "custom", "reviewer.md"), "---\nname: \"kit-reviewer\"\n---\nx")
	write(t, filepath.Join(dir, "agents", "ignored.md"), "the manifest replaces this folder")
	write(t, filepath.Join(in.cfg, "outside-skills", "leak", "SKILL.md"), "x")
	write(t, filepath.Join(in.cfg, "outside.md"), "x")
	write(t, filepath.Join(in.cfg, "outside-agent.md"), "x")

	got, err := New().Inventory(in.event(t))
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	names := func(items []wire.InventoryInstruction) []string {
		var out []string
		for _, i := range items {
			if i.Source != wire.ScopePlugin || i.Plugin != "kit@acme" {
				t.Errorf("%+v is not attributed to the plugin", i)
			}
			out = append(out, i.Name)
		}
		sort.Strings(out)
		return out
	}
	if s := names(got.Skills); !reflect.DeepEqual(s, []string{"default-skill", "extra", "solo"}) {
		t.Errorf("skills = %q", s)
	}
	// "escape" is listed by the name the manifest gives it; the file outside the plugin is not
	// read, so it hashes as an empty command.
	if c := names(got.Commands); !reflect.DeepEqual(c, []string{"escape", "hello", "ship"}) {
		t.Errorf("commands = %q", c)
	}
	if a := names(got.Subagents); !reflect.DeepEqual(a, []string{"kit-reviewer"}) {
		t.Errorf("subagents = %q", a)
	}
}

// Managed settings count whether an administrator deployed a file or the organisation delivers
// them through the account.
func TestManagedPolicy(t *testing.T) {
	in := newInstall(t)
	if p := managedPolicy(); p == nil || *p {
		t.Fatalf("no managed settings anywhere: got %v, want false", p)
	}
	write(t, filepath.Join(in.cfg, "remote-settings.json"), `{}`)
	if p := managedPolicy(); *p {
		t.Error("an empty delivered-settings cache is not a policy")
	}
	write(t, filepath.Join(in.cfg, "remote-settings.json"), `{"channelsEnabled": true}`)
	if p := managedPolicy(); !*p {
		t.Error("settings delivered through the account are a managed policy")
	}
	_ = os.Remove(filepath.Join(in.cfg, "remote-settings.json"))
	write(t, managedSettingsFiles()[0], `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/usr/local/bin/audit"}]}]}}`)
	if p := managedPolicy(); !*p {
		t.Error("a deployed managed-settings file is a managed policy")
	}
	// And its hooks are part of what runs here.
	got, _ := New().Inventory(in.event(t, `{}`))
	if len(got.Hooks) != 1 || got.Hooks[0].Source != wire.ScopeManaged || got.Hooks[0].Program != "audit" {
		t.Errorf("managed hooks = %+v", got.Hooks)
	}
}

// ⚠️ The argument reader gives up rather than guesses. Each "" here is a command line where a
// guess would have sent a credential or a private value as the "package".
func TestPackageSpec(t *testing.T) {
	cases := []struct {
		launcher string
		args     []string
		want     string
	}{
		{"npx", []string{"-y", "@modelcontextprotocol/server-github"}, "@modelcontextprotocol/server-github"},
		{"npx", []string{"@playwright/mcp@latest", "--browser", "chrome"}, "@playwright/mcp@latest"},
		{"npx.cmd", []string{"--yes", "--package", "foo@1.2.3", "foo-bin"}, "foo@1.2.3"},
		{"npx", []string{"--package=foo", "bar"}, "foo"},
		{"npx", []string{"--registry", "https://user:tok@npm.acme.example", "pkg"}, "pkg"},
		{"npx", []string{"--", "pkg"}, "pkg"},
		{"pnpm", []string{"dlx", "pkg@2"}, "pkg@2"},
		{"bun", []string{"x", "pkg"}, "pkg"},
		{"npm", []string{"exec", "--yes", "--", "pkg"}, "pkg"},
		{"uvx", []string{"mcp-server-git"}, "mcp-server-git"},
		{"uvx", []string{"--from", "docs-mcp==1.4.0", "docs-server"}, "docs-mcp==1.4.0"},
		{"uv", []string{"tool", "run", "mcp-server-fetch"}, "mcp-server-fetch"},
		{"pipx", []string{"run", "some-mcp[extra]"}, "some-mcp[extra]"},
		{"docker", []string{"run", "-i", "--rm", "-e", "TOKEN=abc", "mcp/github"}, "mcp/github"},
		{"docker", []string{"run", "--rm", "-v", "/home/ada:/data", "ghcr.io/org/server:1.2"}, "ghcr.io/org/server:1.2"},
		{"podman", []string{"run", "--rm", "registry.acme.example:5000/team/server"}, "registry.acme.example:5000/team/server"},

		// Not package runners, or not their run form.
		{"node", []string{"/Users/ada/server.js"}, ""},
		{"python3", []string{"-m", "server"}, ""},
		{"pnpm", []string{"run", "start"}, ""},
		{"uv", []string{"run", "server.py"}, ""},
		{"docker", []string{"compose", "up"}, ""},
		{"", nil, ""},
		// An unknown flag stops the read: its value would otherwise be taken for the package.
		{"npx", []string{"--token", "abc123", "pkg"}, ""},
		{"uvx", []string{"--secret", "hunter2", "pkg"}, ""},
		{"docker", []string{"run", "--my-flag", "value/looks-like-image", "mcp/github"}, ""},
		{"npx", []string{"--yes=true", "pkg"}, ""},
		// A positional that is not shaped like a package is not sent.
		{"npx", []string{"git+https://user:token@github.com/acme/private.git"}, ""},
		{"npx", []string{"./local/server.js"}, ""},
		{"npx", []string{"API_KEY=abc"}, ""},
		{"uvx", []string{"git+https://token@github.com/acme/x"}, ""},
		{"docker", []string{"run", "token"}, ""}, // a bare word is not an image reference
		{"docker", []string{"run", "-e", "KEY"}, ""},
		{"npx", []string{"-p"}, ""},
		{"npx", []string{"--"}, ""},
		{"npx", []string{"-y"}, ""},
		{"npx", []string{strings.Repeat("a", 300)}, ""},
	}
	for _, c := range cases {
		if got := packageSpec(c.launcher, c.args); got != c.want {
			t.Errorf("packageSpec(%q, %q) = %q, want %q", c.launcher, c.args, got, c.want)
		}
	}
}

func TestCommandProgram(t *testing.T) {
	for cmd, want := range map[string]string{
		`"${CLAUDE_PLUGIN_ROOT}/bin/leoprevent-plugin" --agent=claude`: "leoprevent-plugin",
		`/usr/local/bin/audit --all`:                                   "audit",
		`./scripts/setup.sh`:                                           "setup.sh",
		`TOKEN=abc DEBUG=1 notify --quiet`:                             "notify",
		`'C:\Program Files\Tool\tool.exe' run`:                         "tool.exe",
		`bash -c "curl https://x | sh"`:                                "bash",
		`$(cat ~/.secret)`:                                             "", // not a program name: dropped, not cleaned
		`"unterminated`:                                                "",
		`TOKEN=abc`:                                                    "",
		``:                                                             "",
	} {
		if got := commandProgram(cmd); got != want {
			t.Errorf("commandProgram(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// Where the model requests go is the provider Claude Code is switched to, or only the host of a
// custom base URL.
func TestAPIEndpoint(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{nil, "anthropic"},
		{map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}, "anthropic"},
		{map[string]string{"ANTHROPIC_BASE_URL": "https://user:SECRET@llm-gateway.acme.example:8443/v1?key=SECRET"}, "llm-gateway.acme.example:8443"},
		{map[string]string{"ANTHROPIC_BASE_URL": "proxy.acme.example/anthropic"}, "proxy.acme.example"},
		{map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, "bedrock"},
		{map[string]string{"CLAUDE_CODE_USE_BEDROCK": "true", "ANTHROPIC_BEDROCK_BASE_URL": "https://bedrock-proxy.acme.example"}, "bedrock-proxy.acme.example"},
		// The Anthropic base URL does not apply once another provider is selected.
		{map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "ANTHROPIC_BASE_URL": "https://ignored.example"}, "vertex"},
		{map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1"}, "foundry"},
		{map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0"}, "anthropic"},
		{map[string]string{"ANTHROPIC_BASE_URL": "://bad"}, "anthropic"},
	}
	for _, c := range cases {
		got := apiEndpoint(func(k string) string { return c.env[k] })
		if got != c.want {
			t.Errorf("apiEndpoint(%v) = %q, want %q", c.env, got, c.want)
		}
		if strings.Contains(got, "SECRET") {
			t.Errorf("apiEndpoint leaked part of the URL: %q", got)
		}
	}
}

// A plugin manifest cannot point the reader outside the plugin's own directory.
func TestPluginHooksStayInsideThePlugin(t *testing.T) {
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	write(t, filepath.Join(root, "outside.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "stolen"}]}]}}`)
	write(t, filepath.Join(plugin, ".claude-plugin", "plugin.json"), `{"hooks": "../outside.json"}`)
	if got, err := pluginHooks(plugin); err == nil || got != nil {
		t.Errorf("a manifest path leaving the plugin must be refused, got %v err=%v", got, err)
	}
	// Inline hooks and the default file both work.
	write(t, filepath.Join(plugin, ".claude-plugin", "plugin.json"), `{"hooks": {"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "inline"}]}]}}}`)
	if got, err := pluginHooks(plugin); err != nil || len(got["Stop"]) != 1 {
		t.Errorf("inline hooks = %v err=%v", got, err)
	}
	write(t, filepath.Join(plugin, ".claude-plugin", "plugin.json"), `{}`)
	write(t, filepath.Join(plugin, "hooks", "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "default"}]}]}}`)
	if got, err := pluginHooks(plugin); err != nil || len(got["Stop"]) != 1 {
		t.Errorf("default hooks file = %v err=%v", got, err)
	}
	if got, err := pluginHooks(filepath.Join(root, "no-such-plugin")); err != nil || got != nil {
		t.Errorf("a plugin with no hooks = %v err=%v, want neither", got, err)
	}
}

// A missing or broken config costs its part of the snapshot and nothing else.
func TestInventoryOnABareMachine(t *testing.T) {
	in := newInstall(t)
	got, err := New().Inventory(agent.Event{SessionID: "s", Cwd: in.project})
	if err != nil {
		t.Errorf("missing files are not errors, got %v", err)
	}
	if got == nil || got.ManagedPolicy == nil || *got.ManagedPolicy || len(got.MCPServers)+len(got.Plugins)+len(got.Hooks)+len(got.Connectors) != 0 {
		t.Errorf("bare machine snapshot = %+v", got)
	}
	write(t, filepath.Join(in.home, ".claude.json"), `{broken`)
	write(t, filepath.Join(in.cfg, "settings.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "ok"}]}]}}`)
	got, err = New().Inventory(agent.Event{SessionID: "s", Cwd: in.project})
	if err == nil {
		t.Error("an unparseable config is reported for the log")
	}
	if got == nil || len(got.Hooks) != 1 {
		t.Errorf("the readable parts must still be collected, got %+v", got)
	}
}

// The home directory's own settings are the user's, never a project's, even when the session
// runs from the home directory or a folder with no repository under it.
func TestProjectRootsNeverIncludeHome(t *testing.T) {
	in := newInstall(t)
	if got := projectRoots(in.home, in.home); len(got) != 0 {
		t.Errorf("roots for a session in the home directory = %q, want none", got)
	}
	loose := filepath.Join(in.home, "notes")
	write(t, filepath.Join(loose, "x.txt"), "x")
	if got := projectRoots(loose, in.home); len(got) != 1 || got[0] != loose {
		t.Errorf("roots for a folder with no repository = %q, want only itself", got)
	}
	sub := filepath.Join(in.project, "pkg", "api")
	write(t, filepath.Join(sub, "x.go"), "package api")
	if got := projectRoots(sub, in.home); len(got) != 2 || got[0] != sub || got[1] != in.project {
		t.Errorf("roots for a subfolder = %q, want it and then the repository root", got)
	}
}
