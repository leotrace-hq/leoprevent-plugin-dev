package wire

import "strings"

// AgentInventory is a snapshot of how the coding agent is INSTALLED on the developer's machine
// (LEO-822): the connectors and MCP servers it can reach, the plugins and hooks that run inside
// it, and two facts about the setup around it. It answers "what is installed company-wide", which
// the per-turn dimensions on TurnMeta (agent, model, surface) cannot.
//
// It is sent on a request of its own, POST /inventory (InventoryRequest), and only when the
// snapshot changed or a day has passed since it was last sent (client/internal/inventory). It is
// not part of TurnMeta, so the /review and /telemetry requests never carry it.
//
// ⚠️ METADATA ONLY, AND THE CLIENT DECIDES THAT, NOT THE SERVER. Every field is a name, a version,
// a host or a hash. An MCP server's `env` block and headers, its full argument list, a hook's
// command text, file paths, project names, and everything about the signed-in account except the
// one AccountType flag stay on the machine. A new field here is an egress change: docs/egress.md.
type AgentInventory struct {
	// Connectors are the hosted connectors loaded in the session, used or not (for Claude Code,
	// the claude.ai connectors).
	Connectors []InventoryConnector `json:"connectors,omitempty"`
	// MCPServers are the MCP servers the agent is CONFIGURED with, from its own config files.
	// TurnMeta.MCPServersUsed is the other half: what a turn actually called.
	MCPServers []InventoryMCPServer `json:"mcp_servers,omitempty"`
	Plugins    []InventoryPlugin    `json:"plugins,omitempty"`
	Hooks      []InventoryHook      `json:"hooks,omitempty"`
	// Skills, Commands and Subagents are the instruction files the agent can load: a skill, a
	// custom slash command, a custom subagent. Each is text the agent then follows with the
	// developer's rights, so where one comes from matters as much as a plugin's source does.
	Skills    []InventoryInstruction `json:"skills,omitempty"`
	Commands  []InventoryInstruction `json:"commands,omitempty"`
	Subagents []InventoryInstruction `json:"subagents,omitempty"`
	// Programs are the programs the agent's settings make it start by itself, other than hooks:
	// the one that draws its status line, the one that fetches its API key.
	Programs []InventoryProgram `json:"programs,omitempty"`
	// AccountType is one of the Account* values: whether the agent is signed in with a company
	// account or a personal one. A flag only. The account's email, organisation name and billing
	// tier are never read into the snapshot.
	AccountType string `json:"account_type,omitempty"`
	// ManagedPolicy reports whether centrally managed settings apply on this machine. A pointer
	// so "no" is sent and stays distinct from an adapter that could not tell.
	ManagedPolicy *bool `json:"managed_policy,omitempty"`
	// AllowsAnyCommand reports whether a standing permission rule lets the agent run every shell
	// command without asking. TurnMeta.PermissionMode cannot show it: a session in the careful
	// default mode still asks about nothing when such a rule is set. A flag only; the rules
	// themselves hold paths and commands and stay on the machine. A pointer so "no" is sent.
	AllowsAnyCommand *bool `json:"allows_any_command,omitempty"`
}

// InventoryRequest is the POST /inventory body (cloud tier): one installation snapshot and the
// few facts that say which installation it describes and whose it is. The server appends one
// audit record and answers 202; it does no model work.
//
// ⚠️ A REQUEST OF ITS OWN, NOT A FIELD ON THE TURN. The snapshot is about the installation, not
// about a turn, so it does not ride /review, where the developer is waiting on the answer. That
// also leaves the client free to send it from another hook than Stop.
//
// ⚠️ NARROW ON PURPOSE. It carries no prompt, no code, no token counts and no model name: only
// the fields below. A new field here is an egress change: docs/egress.md.
type InventoryRequest struct {
	// InstallID names the installation (see TurnMeta.InstallID). The snapshot is kept by it.
	InstallID string `json:"install_id,omitempty"`
	// Agent is the coding agent the snapshot describes ("claude"), and AgentVersion its own
	// version as the turn stamped it.
	Agent        string `json:"agent,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	// Repo is the repository the session was in. Part of a snapshot comes from that repository's
	// own files (its MCP servers, hooks and skills), so a reader needs it to place those.
	Repo string `json:"repo,omitempty"`
	// Developer and DeveloperSource are as on TurnMeta: who the installation is attributed to.
	Developer       string `json:"developer,omitempty"`
	DeveloperSource string `json:"developer_source,omitempty"`
	// OS, Arch, ClientVersion, Environment and EnvironmentRaw are as on TurnMeta.
	OS             string `json:"os,omitempty"`
	Arch           string `json:"arch,omitempty"`
	ClientVersion  string `json:"client_version,omitempty"`
	Environment    string `json:"environment,omitempty"`
	EnvironmentRaw string `json:"environment_raw,omitempty"`
	// Inventory is the snapshot. A request without one is refused: there is nothing to record.
	Inventory *AgentInventory `json:"inventory,omitempty"`
}

// InventoryResponse is the /inventory ack: the record was appended.
type InventoryResponse struct {
	Accepted bool `json:"accepted"`
}

// Account* are the AgentInventory.AccountType values.
const (
	AccountCompany  = "company"
	AccountPersonal = "personal"
	// AccountAPIKey is an agent that authenticates with an API key instead of a signed-in
	// account. The key and where it comes from are never read into the snapshot.
	AccountAPIKey = "api_key"
	// AccountUnknown is an account whose kind the client did not recognise. Distinct from an
	// empty AccountType, which means no signed-in account was found (an API key, a cloud provider).
	AccountUnknown = "unknown"
)

// InventoryConnector is one hosted connector.
type InventoryConnector struct {
	Name string `json:"name"`
	// ID is the server name the agent's transcript uses for it, when that differs from Name: the
	// Claude desktop app names a connector by an opaque id, so this is what joins
	// TurnMeta.MCPServersUsed to a readable name.
	ID string `json:"id,omitempty"`
	// Host is the connector's host, when the machine has it. Never a path or a query string.
	Host string `json:"host,omitempty"`
	// Auth is one of the Connector* values: whether the developer has signed in to the connector,
	// so the agent can read and write through it, or it is only on offer. Empty when the session
	// does not say.
	Auth string `json:"auth,omitempty"`
}

// Connector* are the InventoryConnector.Auth values.
const (
	ConnectorSignedIn = "signed_in"
	// ConnectorNotSignedIn is a connector the agent lists but holds no access to: all it can do
	// with it is offer the developer the sign-in.
	ConnectorNotSignedIn = "not_signed_in"
)

// InventoryMCPServer is one configured MCP server.
type InventoryMCPServer struct {
	Name string `json:"name"`
	// Scope is where it is configured: one of the Scope* values.
	Scope string `json:"scope,omitempty"`
	// Transport is "stdio" for a local program, else the remote transport ("http", "sse").
	Transport string `json:"transport,omitempty"`
	// Launcher is the base name of the program a stdio server starts ("npx", "uvx", "docker"),
	// never its path.
	Launcher string `json:"launcher,omitempty"`
	// Package is the package or image the launcher runs ("@playwright/mcp@latest"), only when
	// the launcher is a known package runner and the argument is shaped like a package. It is
	// what a vulnerability lookup needs, and the only argument that ever leaves the machine.
	Package string `json:"package,omitempty"`
	// Host is a remote server's host. Never a path, a query string or a header.
	Host string `json:"host,omitempty"`
}

// InventoryPlugin is one installed agent plugin.
type InventoryPlugin struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace,omitempty"`
	Version     string `json:"version,omitempty"`
	// Commit is the git commit the plugin was installed at.
	Commit  string `json:"commit,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Enabled bool   `json:"enabled"`
}

// InventoryHook is one configured hook.
type InventoryHook struct {
	Event string `json:"event"`
	// Source is where it is defined: a Scope* value, or ScopePlugin with Plugin naming which.
	Source string `json:"source,omitempty"`
	Plugin string `json:"plugin,omitempty"`
	// Program is the base name of the program a command hook starts, or the hook's type for a
	// hook that runs no command.
	Program string `json:"program,omitempty"`
	// Hash is a short hash of the hook's full command, home directory normalised, so the same
	// hook on two machines compares equal while the command text stays on the machine.
	Hash string `json:"hash,omitempty"`
}

// InventoryInstruction is one skill, custom slash command or custom subagent.
type InventoryInstruction struct {
	// Name is the name the agent knows it by: the skill's folder, the command's or subagent's
	// file without its extension. Never a path.
	Name string `json:"name"`
	// Source is where it is installed: a Scope* value, or ScopePlugin with Plugin naming which.
	Source string `json:"source,omitempty"`
	Plugin string `json:"plugin,omitempty"`
	// Hash is a short hash of the instruction file, so the same skill on two machines compares
	// equal and a changed one stands out, while its text stays on the machine.
	Hash string `json:"hash,omitempty"`
}

// InventoryProgram is one program the agent's settings make it start by itself.
type InventoryProgram struct {
	// Kind is one of the Program* values.
	Kind string `json:"kind"`
	// Source is the settings file it is set in: a Scope* value.
	Source string `json:"source,omitempty"`
	// Program is the base name of the program, never its path or arguments.
	Program string `json:"program,omitempty"`
	// Hash is a short hash of the full command, home directory normalised, as for a hook.
	Hash string `json:"hash,omitempty"`
}

// Program* are the InventoryProgram.Kind values.
const (
	ProgramStatusLine   = "status_line"
	ProgramAPIKeyHelper = "api_key_helper"
)

// Scope* say where an inventory item is configured.
const (
	// ScopeAccount is an item the signed-in account brings with it, on every machine it is used on.
	ScopeAccount = "account"
	ScopeUser    = "user"
	ScopeProject = "project" // checked into the repository
	ScopeLocal   = "local"   // this project on this machine only
	ScopeManaged = "managed"
	ScopePlugin  = "plugin"
)

// Inventory bounds. A config file is developer-authored, so nothing in it is trusted to be small.
const (
	maxInventoryItems = 200
	maxInventoryText  = 200
	maxServersUsed    = 50
)

// ValidInventory returns inv with every list and string bounded, and nil for a snapshot with
// nothing in it. Both sides call it: the client before sending, and the server before storing,
// since the server must not trust the client's bounds.
func ValidInventory(inv *AgentInventory) *AgentInventory {
	if inv == nil {
		return nil
	}
	out := AgentInventory{ManagedPolicy: inv.ManagedPolicy, AllowsAnyCommand: inv.AllowsAnyCommand}
	switch inv.AccountType {
	case AccountCompany, AccountPersonal, AccountAPIKey, AccountUnknown:
		out.AccountType = inv.AccountType
	}
	for _, c := range capItems(inv.Connectors) {
		if c.Name = capText(c.Name); c.Name != "" {
			c.ID, c.Host = capText(c.ID), capText(c.Host)
			if c.Auth != ConnectorSignedIn && c.Auth != ConnectorNotSignedIn {
				c.Auth = ""
			}
			out.Connectors = append(out.Connectors, c)
		}
	}
	for _, s := range capItems(inv.MCPServers) {
		if s.Name = capText(s.Name); s.Name != "" {
			s.Scope, s.Transport = capText(s.Scope), capText(s.Transport)
			s.Launcher, s.Package, s.Host = capText(s.Launcher), capText(s.Package), capText(s.Host)
			out.MCPServers = append(out.MCPServers, s)
		}
	}
	for _, p := range capItems(inv.Plugins) {
		if p.Name = capText(p.Name); p.Name != "" {
			p.Marketplace, p.Version = capText(p.Marketplace), capText(p.Version)
			p.Commit, p.Scope = capText(p.Commit), capText(p.Scope)
			out.Plugins = append(out.Plugins, p)
		}
	}
	for _, h := range capItems(inv.Hooks) {
		if h.Event = capText(h.Event); h.Event != "" {
			h.Source, h.Plugin = capText(h.Source), capText(h.Plugin)
			h.Program, h.Hash = capText(h.Program), capText(h.Hash)
			out.Hooks = append(out.Hooks, h)
		}
	}
	out.Skills = validInstructions(inv.Skills)
	out.Commands = validInstructions(inv.Commands)
	out.Subagents = validInstructions(inv.Subagents)
	for _, p := range capItems(inv.Programs) {
		if p.Kind == ProgramStatusLine || p.Kind == ProgramAPIKeyHelper {
			p.Source, p.Program, p.Hash = capText(p.Source), capText(p.Program), capText(p.Hash)
			out.Programs = append(out.Programs, p)
		}
	}
	if out.AccountType == "" && out.ManagedPolicy == nil && out.AllowsAnyCommand == nil &&
		len(out.Connectors) == 0 && len(out.MCPServers) == 0 && len(out.Plugins) == 0 && len(out.Hooks) == 0 &&
		len(out.Skills) == 0 && len(out.Commands) == 0 && len(out.Subagents) == 0 && len(out.Programs) == 0 {
		return nil
	}
	return &out
}

func validInstructions(items []InventoryInstruction) []InventoryInstruction {
	var out []InventoryInstruction
	for _, i := range capItems(items) {
		if i.Name = capText(i.Name); i.Name != "" {
			i.Source, i.Plugin, i.Hash = capText(i.Source), capText(i.Plugin), capText(i.Hash)
			out = append(out, i)
		}
	}
	return out
}

// ValidServersUsed bounds TurnMeta.MCPServersUsed the same way.
func ValidServersUsed(names []string) []string {
	var out []string
	for _, n := range names {
		if len(out) == maxServersUsed {
			break
		}
		if n = capText(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ValidSkillsUsed bounds TurnMeta.SkillsUsed the same way.
func ValidSkillsUsed(names []string) []string { return ValidServersUsed(names) }

// ValidInventoryText bounds one of TurnMeta's single inventory values (AgentVersion,
// PermissionMode, APIEndpoint).
func ValidInventoryText(s string) string { return capText(s) }

// InstallIDLen is the length of TurnMeta.InstallID: 16 random bytes in lowercase hex.
const InstallIDLen = 32

// ValidInstallID returns s when it has the shape of an install id, and "" otherwise. The shape is
// all that can be checked: the value is random.
func ValidInstallID(s string) string {
	if len(s) != InstallIDLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}

func capItems[T any](items []T) []T {
	if len(items) > maxInventoryItems {
		return items[:maxInventoryItems]
	}
	return items
}

// capText trims s, drops it when it holds a control character (a name or a version never does,
// so one that does is not what the field is for), and bounds its length on a rune boundary.
func capText(s string) string {
	s = strings.TrimSpace(s)
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	if len(s) <= maxInventoryText {
		return s
	}
	cut := maxInventoryText
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
