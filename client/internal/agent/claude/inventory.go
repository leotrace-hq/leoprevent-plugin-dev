package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/transcript"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// This file is Claude Code's half of the agent inventory (LEO-822): agent.Inventorier.
//
// ⚠️ THE FILES READ HERE HOLD SECRETS, AND THE STRUCTS BELOW ARE THE GUARD. `~/.claude.json`
// carries the signed-in account and every MCP server's `env` block and headers, where tokens
// live. Each file is decoded into a struct that DECLARES ONLY THE FIELDS THE SNAPSHOT NEEDS, so
// an `env` or `headers` value is never held in a Go value at all, let alone copied. Adding a
// field to one of these structs is how a secret would start to leave the machine: check it
// against wire.AgentInventory and docs/egress.md § Agent inventory first.
//
// Everything is best-effort. A file that is missing, too large or not JSON costs its part of the
// snapshot and nothing else.

// Overridable in tests.
var (
	userHomeDir   = os.UserHomeDir
	userConfigDir = os.UserConfigDir
	// managedSettingsFiles are the machine-wide files an administrator deploys Claude Code's
	// managed settings in. The first is JSON and is also read for its hooks and plugins; the
	// rest only prove a policy is deployed.
	managedSettingsFiles = defaultManagedSettingsFiles
)

func defaultManagedSettingsFiles() []string {
	switch runtime.GOOS {
	case "darwin":
		files := []string{
			"/Library/Application Support/ClaudeCode/managed-settings.json",
			"/Library/Managed Preferences/com.anthropic.claudecode.plist",
		}
		if home, err := userHomeDir(); err == nil && home != "" {
			files = append(files, filepath.Join("/Library/Managed Preferences", filepath.Base(home), "com.anthropic.claudecode.plist"))
		}
		return files
	case "windows":
		return []string{
			`C:\Program Files\ClaudeCode\managed-settings.json`,
			`C:\ProgramData\ClaudeCode\managed-settings.json`,
		}
	default:
		return []string{"/etc/claude-code/managed-settings.json"}
	}
}

// maxConfigBytes bounds every file read here. `~/.claude.json` grows with the developer's
// project history and is routinely a few megabytes; anything past this is not a config file.
const maxConfigBytes = 32 << 20

// readJSON decodes a regular file of at most maxConfigBytes into v. Symlinks are followed: a
// dotfile manager links an agent's settings into place, and these are the agent's own files.
func readJSON(path string, v any) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxConfigBytes {
		return fmt.Errorf("%s: not a readable config file", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// configDir is Claude Code's configuration directory, and globalConfigPath its global state
// file, which sits INSIDE the directory when $CLAUDE_CONFIG_DIR moves it and beside the default
// one otherwise.
func configDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	if home, err := userHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".claude")
	}
	return ""
}

func globalConfigPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	if home, err := userHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".claude.json")
	}
	return ""
}

// mcpServerConfig is one configured MCP server. NO `env`, NO `headers`: see the file note.
type mcpServerConfig struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	URL     string   `json:"url"`
}

// globalConfig is the part of `~/.claude.json` the snapshot reads.
type globalConfig struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]mcpServerConfig `json:"mcpServers"`
	} `json:"projects"`
	// OAuthAccount: the organisation TYPE only. The account's email, names, ids and billing
	// fields sit beside it and are deliberately not declared.
	OAuthAccount *struct {
		OrganizationType string `json:"organizationType"`
	} `json:"oauthAccount"`
	// EverConnected lists the claude.ai connectors this install has connected, by readable name.
	// Used only to spell a connector's name back out of a tool name; it is not itself sent.
	EverConnected []string `json:"claudeAiMcpEverConnected"`
}

// settingsFile is the part of a Claude Code settings file the snapshot reads.
type settingsFile struct {
	Hooks          map[string][]hookGroup     `json:"hooks"`
	EnabledPlugins map[string]json.RawMessage `json:"enabledPlugins"`
	// The three below are kept raw and decoded on their own, so a shape this code does not
	// expect costs that one setting and not the file's hooks and plugins. Permissions is read
	// for one yes/no answer (allowsAnyCommand); the two others name a program (programs).
	Permissions  json.RawMessage `json:"permissions"`
	StatusLine   json.RawMessage `json:"statusLine"`
	APIKeyHelper json.RawMessage `json:"apiKeyHelper"`
}

type hookGroup struct {
	Hooks []hookEntry `json:"hooks"`
}

// hookEntry is one hook. Its text is read to be hashed and to name the program it starts, and
// goes no further.
type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Prompt  string `json:"prompt"`
	URL     string `json:"url"`
}

// APIEndpoint says where Claude Code sends its model requests, from the same process
// environment the hook inherits: the provider it is switched to, or the host of a custom base
// URL (a company gateway or a proxy). Never more of the URL than the host.
func (*Adapter) APIEndpoint() string { return apiEndpoint(os.Getenv) }

func apiEndpoint(getenv func(string) string) string {
	on := func(key string) bool {
		switch strings.ToLower(strings.TrimSpace(getenv(key))) {
		case "", "0", "false", "no", "off":
			return false
		}
		return true
	}
	provider, base := "anthropic", getenv("ANTHROPIC_BASE_URL")
	switch {
	case on("CLAUDE_CODE_USE_BEDROCK"):
		provider, base = "bedrock", getenv("ANTHROPIC_BEDROCK_BASE_URL")
	case on("CLAUDE_CODE_USE_VERTEX"):
		provider, base = "vertex", getenv("ANTHROPIC_VERTEX_BASE_URL")
	case on("CLAUDE_CODE_USE_FOUNDRY"):
		provider, base = "foundry", getenv("ANTHROPIC_FOUNDRY_BASE_URL")
	}
	if host := hostOf(base); host != "" && host != "api.anthropic.com" {
		return host
	}
	return provider
}

// hostOf returns the host (and port) of a URL, lower-cased, and nothing else of it: no scheme,
// no credentials, no path, no query. "" when raw names no host.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	// Only something shaped like a host leaves. A malformed URL can otherwise put part of a
	// credential here ("https://tok/en@host" parses with the host "tok"), and a value that is
	// not a URL at all would be sent whole.
	if !hostRe.MatchString(host) || (!strings.Contains(host, ".") && !strings.HasPrefix(host, "localhost") && !strings.HasPrefix(host, "[")) {
		return ""
	}
	return host
}

// hostRe is what a host (with an optional port) may look like to be sent: a DNS name, an IPv4
// address or a bracketed IPv6 address.
var hostRe = regexp.MustCompile(`^(\[[0-9a-f:.]+\]|[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?)(:[0-9]{1,5})?$`)

// Inventory reads how this Claude Code install is set up. See agent.Inventorier.
func (*Adapter) Inventory(ev agent.Event) (*wire.AgentInventory, error) {
	var errs []error
	note := func(what string, err error) {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}
	home, _ := userHomeDir()
	roots := projectRoots(ev.Cwd, home)

	var global globalConfig
	if p := globalConfigPath(); p != "" {
		note("global config", readJSON(p, &global))
	}
	layers := settingsLayers(roots, note)

	inv := &wire.AgentInventory{
		MCPServers:       mcpServers(global, roots, note),
		AccountType:      accountType(global, layers, os.Getenv),
		ManagedPolicy:    managedPolicy(),
		AllowsAnyCommand: allowsAnyCommand(layers),
		Programs:         programs(layers, home),
	}
	var enabled []enabledPlugin
	inv.Plugins, enabled = plugins(layers, roots, note)
	inv.Hooks = hooks(layers, enabled, home, note)
	inv.Skills, inv.Commands, inv.Subagents = instructions(roots, enabled)
	connectors, err := sessionConnectors(ev, global)
	note("connectors", err)
	inv.Connectors = connectors
	return inv, errors.Join(errs...)
}

// projectRoots are the directories Claude Code's project-level files are looked for in: the
// working directory, then the root of the repository holding it. The home directory is never
// one, so `~/.claude/settings.json` is not mistaken for a project's.
func projectRoots(cwd, home string) []string {
	if cwd == "" {
		return nil
	}
	var roots []string
	add := func(dir string) {
		if dir == "" || (home != "" && filepath.Clean(dir) == filepath.Clean(home)) {
			return
		}
		for _, r := range roots {
			if r == dir {
				return
			}
		}
		roots = append(roots, dir)
	}
	add(filepath.Clean(cwd))
	for dir := filepath.Clean(cwd); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			add(dir)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || (home != "" && dir == filepath.Clean(home)) {
			break
		}
		dir = parent
	}
	return roots
}

// firstExisting returns the first of roots that holds rel.
func firstExisting(roots []string, rel string) string {
	for _, r := range roots {
		p := filepath.Join(r, rel)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// settingsLayer is one settings file and where it applies.
type settingsLayer struct {
	scope    string
	settings settingsFile
}

// settingsLayers reads the settings files in the order Claude Code applies them, lowest
// precedence first: user, project, local, managed.
func settingsLayers(roots []string, note func(string, error)) []settingsLayer {
	var layers []settingsLayer
	read := func(scope, path string) {
		if path == "" {
			return
		}
		var s settingsFile
		if err := readJSON(path, &s); err != nil {
			note(scope+" settings", err)
			return
		}
		layers = append(layers, settingsLayer{scope: scope, settings: s})
	}
	if dir := configDir(); dir != "" {
		read(wire.ScopeUser, filepath.Join(dir, "settings.json"))
	}
	read(wire.ScopeProject, firstExisting(roots, filepath.Join(".claude", "settings.json")))
	read(wire.ScopeLocal, firstExisting(roots, filepath.Join(".claude", "settings.local.json")))
	for _, f := range managedSettingsFiles() {
		if strings.HasSuffix(f, ".json") {
			read(wire.ScopeManaged, f)
		}
	}
	return layers
}

// mcpServers lists the MCP servers Claude Code is configured with for this project: the user's
// own, this project's private ones (both in the global config), and the ones the repository
// checks in as `.mcp.json`.
func mcpServers(global globalConfig, roots []string, note func(string, error)) []wire.InventoryMCPServer {
	var out []wire.InventoryMCPServer
	add := func(scope string, servers map[string]mcpServerConfig) {
		for name, c := range servers {
			out = append(out, mcpServer(name, scope, c))
		}
	}
	add(wire.ScopeUser, global.MCPServers)
	for _, r := range roots {
		// The global config keys a project by its path, spelled with forward slashes on every OS.
		if p, ok := global.Projects[filepath.ToSlash(r)]; ok {
			add(wire.ScopeLocal, p.MCPServers)
			break
		}
	}
	if p := firstExisting(roots, ".mcp.json"); p != "" {
		var f struct {
			MCPServers map[string]mcpServerConfig `json:"mcpServers"`
		}
		if err := readJSON(p, &f); err != nil {
			note("project mcp config", err)
		}
		add(wire.ScopeProject, f.MCPServers)
	}
	return out
}

func mcpServer(name, scope string, c mcpServerConfig) wire.InventoryMCPServer {
	s := wire.InventoryMCPServer{Name: name, Scope: scope}
	if c.URL != "" || (c.Type != "" && c.Type != "stdio") {
		s.Transport = c.Type
		if s.Transport == "" {
			s.Transport = "http"
		}
		s.Host = hostOf(c.URL)
		return s
	}
	s.Transport = "stdio"
	// With an args list the command is the program alone, and its path may hold spaces
	// ("C:\Users\Jane Doe\npm\server.cmd"): it is never split, or a folder name would be
	// sent as the launcher.
	if len(c.Args) > 0 {
		s.Launcher = programName(strings.TrimSpace(c.Command))
		s.Package = packageSpec(s.Launcher, c.Args)
		return s
	}
	// A command written as one string ("npx -y pkg") carries its arguments with it.
	fields := strings.Fields(c.Command)
	if len(fields) == 0 {
		return s
	}
	// One string that starts with a path and goes on cannot be told apart from a path with a
	// space in it, so nothing is taken from it.
	if len(fields) > 1 && strings.ContainsAny(fields[0], `/\`) {
		return s
	}
	s.Launcher = programName(fields[0])
	s.Package = packageSpec(s.Launcher, fields[1:])
	return s
}

// programRe is what a program's base name may look like to be sent. Anything else (a shell
// substitution, a quoted string, a value with an equals sign) is dropped rather than cleaned.
var programRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// programName is the base name of a program path, or "" when it is not shaped like one.
func programName(path string) string {
	path = strings.Trim(path, `"'`)
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	if !programRe.MatchString(path) {
		return ""
	}
	return path
}

// Package shapes. Each is deliberately narrow: a value that is not plainly a package name (a
// URL, a path, an assignment, anything with a character a registry would refuse) is not sent.
var (
	npmSpecRe    = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*(@[A-Za-z0-9][A-Za-z0-9._+-]*)?$`)
	pythonSpecRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?(\[[A-Za-z0-9,._-]+\])?((==|>=|<=|~=|@)[A-Za-z0-9][A-Za-z0-9.*+_-]*)?$`)
	// An image must name a repository path ("mcp/github", "ghcr.io/org/name"): a bare word is
	// not accepted, since that is also what an unrecognised flag's value looks like.
	imageRefRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(:[0-9]+)?(/[a-z0-9][a-z0-9._-]*)+(:[A-Za-z0-9_][A-Za-z0-9._-]*)?(@sha256:[0-9a-f]{64})?$`)
)

// runner describes how one package runner's command line is read.
type runner struct {
	spec *regexp.Regexp
	// lead are the subcommand words that come before the package ("dlx", "run", "tool run").
	lead [][]string
	// flags that take no value, flags whose value is skipped, and flags whose value IS the package.
	bools, values, pkg map[string]bool
}

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

var (
	npmBools  = set("-y", "--yes", "-n", "--no", "--no-install", "-q", "--quiet", "--ignore-existing", "--prefer-offline", "--prefer-online", "--offline", "--silent")
	npmValues = set("-c", "--call", "--cache", "--registry", "--userconfig", "--shell")
	npmPkg    = set("-p", "--package")
	pyBools   = set("-q", "--quiet", "-v", "--verbose", "--isolated", "--no-cache", "-n", "--refresh", "--offline", "--native-tls", "--no-progress")
	pyValues  = set("--with", "--python", "-p", "--index-url", "--extra-index-url", "--index", "--default-index", "--cache-dir", "--spec")
	pyPkg     = set("--from")

	runners = map[string]runner{
		"npx":  {spec: npmSpecRe, bools: npmBools, values: npmValues, pkg: npmPkg},
		"bunx": {spec: npmSpecRe, bools: npmBools, values: npmValues, pkg: npmPkg},
		"pnpx": {spec: npmSpecRe, bools: npmBools, values: npmValues, pkg: npmPkg},
		"pnpm": {spec: npmSpecRe, bools: npmBools, values: npmValues, pkg: npmPkg, lead: [][]string{{"dlx"}}},
		"yarn": {spec: npmSpecRe, bools: npmBools, values: npmValues, pkg: npmPkg, lead: [][]string{{"dlx"}}},
		"bun":  {spec: npmSpecRe, bools: npmBools, values: npmValues, pkg: npmPkg, lead: [][]string{{"x"}}},
		"npm":  {spec: npmSpecRe, bools: npmBools, values: npmValues, pkg: npmPkg, lead: [][]string{{"exec"}, {"x"}}},
		"uvx":  {spec: pythonSpecRe, bools: pyBools, values: pyValues, pkg: pyPkg},
		"uv":   {spec: pythonSpecRe, bools: pyBools, values: pyValues, pkg: pyPkg, lead: [][]string{{"tool", "run"}}},
		"pipx": {spec: pythonSpecRe, bools: pyBools, values: pyValues, pkg: set("--spec"), lead: [][]string{{"run"}}},
		"docker": {spec: imageRefRe, lead: [][]string{{"run"}},
			bools: set("-i", "-t", "-it", "-ti", "--rm", "-d", "--detach", "--init", "--interactive", "--tty", "--privileged", "--read-only", "-P", "--publish-all", "-q", "--quiet", "--no-healthcheck"),
			values: set("-e", "--env", "--env-file", "-v", "--volume", "--mount", "-p", "--publish", "--name", "--network", "--net",
				"-w", "--workdir", "-u", "--user", "-l", "--label", "--entrypoint", "-h", "--hostname", "--platform", "--pull",
				"-m", "--memory", "--cpus", "--add-host", "--dns", "--cap-add", "--cap-drop", "--security-opt", "--restart",
				"--tmpfs", "--device", "--gpus", "--log-driver", "--log-opt", "--shm-size", "--ulimit", "--volumes-from")},
	}
)

func init() { runners["podman"] = runners["docker"] }

// packageSpec picks the package (or image) a known package runner is asked to run out of its
// arguments, or "" when it cannot be sure.
//
// ⚠️ IT GIVES UP RATHER THAN GUESSES, BECAUSE ARGUMENTS HOLD SECRETS. `docker run -e TOKEN=…`
// and `npx server --api-key …` are how MCP servers are handed credentials. So it reads flags
// only up to the first positional argument, skips the value of every flag it knows takes one,
// stops at ANY flag it does not know (its value would otherwise be read as the package), and
// accepts the positional only when it is shaped like a package. Whatever follows the package is
// the server's own arguments and is never looked at.
func packageSpec(launcher string, args []string) string {
	name := strings.ToLower(launcher)
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		name = strings.TrimSuffix(name, ext)
	}
	r, ok := runners[name]
	if !ok {
		return ""
	}
	if len(r.lead) > 0 {
		matched := false
		for _, lead := range r.lead {
			if len(args) >= len(lead) && equal(args[:len(lead)], lead) {
				args, matched = args[len(lead):], true
				break
			}
		}
		if !matched {
			return ""
		}
	}
	accept := func(v string) string {
		if len(v) <= 200 && r.spec.MatchString(v) {
			return v
		}
		return ""
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return accept(args[i+1])
			}
			return ""
		case strings.HasPrefix(a, "-"):
			flag, value, inline := strings.Cut(a, "=")
			switch {
			case r.pkg[flag]:
				if inline {
					return accept(value)
				}
				if i+1 < len(args) {
					return accept(args[i+1])
				}
				return ""
			case r.values[flag]:
				if !inline {
					i++
				}
			case r.bools[flag] && !inline:
			default:
				return ""
			}
		default:
			return accept(a)
		}
	}
	return ""
}

func equal(a, b []string) bool {
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// accountType says how Claude Code authenticates, as the one flag the snapshot carries: with an
// API key, or with a signed-in account of one organisation type. "" when it finds neither (a
// cloud provider's own credentials, say).
//
// A key outranks a signed-in account in Claude Code's own order (its authentication docs:
// ANTHROPIC_AUTH_TOKEN, then ANTHROPIC_API_KEY, then apiKeyHelper, then the account), so it is
// checked first. Only whether a key is SET is looked at, never its value. One case reads wrong:
// an interactive session whose developer declined the key in the environment still runs on the
// account, and Claude Code documents nowhere on disk that says so.
func accountType(global globalConfig, layers []settingsLayer, getenv func(string) string) string {
	if getenv("ANTHROPIC_AUTH_TOKEN") != "" || getenv("ANTHROPIC_API_KEY") != "" {
		return wire.AccountAPIKey
	}
	for _, l := range layers {
		var helper string
		if json.Unmarshal(l.settings.APIKeyHelper, &helper) == nil && strings.TrimSpace(helper) != "" {
			return wire.AccountAPIKey
		}
	}
	if global.OAuthAccount == nil {
		return ""
	}
	switch global.OAuthAccount.OrganizationType {
	case "claude_team", "claude_enterprise":
		return wire.AccountCompany
	case "claude_pro", "claude_max", "claude_free":
		return wire.AccountPersonal
	}
	return wire.AccountUnknown
}

// blanketBash are the permission rules that match every shell command (Claude Code's permissions
// docs: a bare `Bash` "matches all Bash commands", and `Bash(*)` is equivalent).
var blanketBash = set("Bash", "Bash(*)")

// allowsAnyCommand reports whether the settings let the agent run every shell command without
// asking: some file allows all of Bash and none denies it, a deny from any file beating an allow
// from any other. The rules are read for this one answer and go no further.
func allowsAnyCommand(layers []settingsLayer) *bool {
	allowed, denied := false, false
	for _, l := range layers {
		var p struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		}
		if len(l.settings.Permissions) == 0 || json.Unmarshal(l.settings.Permissions, &p) != nil {
			continue
		}
		for _, rule := range p.Allow {
			allowed = allowed || blanketBash[strings.TrimSpace(rule)]
		}
		for _, rule := range p.Deny {
			denied = denied || blanketBash[strings.TrimSpace(rule)]
		}
	}
	blanket := allowed && !denied
	return &blanket
}

// programs lists the programs the settings make Claude Code start by itself, other than hooks:
// the status line's command and the API key helper. Named and hashed exactly as a hook is.
func programs(layers []settingsLayer, home string) []wire.InventoryProgram {
	var out []wire.InventoryProgram
	add := func(kind, scope, command string) {
		if strings.TrimSpace(command) == "" {
			return
		}
		h := hook("", scope, "", hookEntry{Type: "command", Command: command}, home)
		out = append(out, wire.InventoryProgram{Kind: kind, Source: scope, Program: h.Program, Hash: h.Hash})
	}
	for _, l := range layers {
		var line struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(l.settings.StatusLine, &line) == nil {
			add(wire.ProgramStatusLine, l.scope, line.Command)
		}
		var helper string
		if json.Unmarshal(l.settings.APIKeyHelper, &helper) == nil {
			add(wire.ProgramAPIKeyHelper, l.scope, helper)
		}
	}
	return out
}

// managedPolicy reports whether centrally managed settings apply here: a machine-wide file an
// administrator deployed, or settings the organisation delivers through the account, which
// Claude Code caches in its config directory. A policy set only in the Windows registry is not
// seen.
func managedPolicy() *bool {
	found := false
	for _, f := range managedSettingsFiles() {
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			found = true
		}
	}
	if dir := configDir(); dir != "" && !found {
		var remote map[string]json.RawMessage
		if readJSON(filepath.Join(dir, "remote-settings.json"), &remote) == nil && len(remote) > 0 {
			found = true
		}
	}
	return &found
}

// enabledPlugin is an enabled plugin and where it is installed, for reading its hooks.
type enabledPlugin struct {
	key, path string
}

// plugins lists the installed plugins that apply to this project, and which are switched on.
func plugins(layers []settingsLayer, roots []string, note func(string, error)) ([]wire.InventoryPlugin, []enabledPlugin) {
	dir := configDir()
	if dir == "" {
		return nil, nil
	}
	var file struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if err := readJSON(filepath.Join(dir, "plugins", "installed_plugins.json"), &file); err != nil {
		note("installed plugins", err)
		return nil, nil
	}
	// A later settings layer overrides an earlier one, plugin by plugin.
	on := map[string]bool{}
	for _, l := range layers {
		for key, raw := range l.settings.EnabledPlugins {
			var v bool
			if json.Unmarshal(raw, &v) == nil {
				on[key] = v
			}
		}
	}
	type install struct {
		Scope        string `json:"scope"`
		Version      string `json:"version"`
		GitCommitSha string `json:"gitCommitSha"`
		InstallPath  string `json:"installPath"`
		ProjectPath  string `json:"projectPath"`
	}
	keys := make([]string, 0, len(file.Plugins))
	for k := range file.Plugins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []wire.InventoryPlugin
	var enabled []enabledPlugin
	for _, key := range keys {
		// One install per scope; older files hold a single object instead of a list.
		var installs []install
		if json.Unmarshal(file.Plugins[key], &installs) != nil {
			var one install
			if json.Unmarshal(file.Plugins[key], &one) != nil {
				continue
			}
			installs = []install{one}
		}
		name, marketplace := key, ""
		if i := strings.LastIndex(key, "@"); i > 0 {
			name, marketplace = key[:i], key[i+1:]
		}
		for _, in := range installs {
			// A plugin installed for another project is not part of this one's set-up.
			if in.ProjectPath != "" && !inRoots(in.ProjectPath, roots) {
				continue
			}
			out = append(out, wire.InventoryPlugin{
				Name: name, Marketplace: marketplace, Version: in.Version,
				Commit: in.GitCommitSha, Scope: in.Scope, Enabled: on[key],
			})
			if on[key] && in.InstallPath != "" {
				enabled = append(enabled, enabledPlugin{key: key, path: in.InstallPath})
			}
		}
	}
	return out, enabled
}

func inRoots(path string, roots []string) bool {
	for _, r := range roots {
		if filepath.Clean(path) == r {
			return true
		}
	}
	return false
}

// hooks lists every hook that runs in this project: the ones in the settings files, and the
// ones each enabled plugin brings.
func hooks(layers []settingsLayer, enabled []enabledPlugin, home string, note func(string, error)) []wire.InventoryHook {
	var out []wire.InventoryHook
	add := func(source, plugin string, byEvent map[string][]hookGroup) {
		for event, groups := range byEvent {
			for _, g := range groups {
				for _, h := range g.Hooks {
					out = append(out, hook(event, source, plugin, h, home))
				}
			}
		}
	}
	for _, l := range layers {
		add(l.scope, "", l.settings.Hooks)
	}
	for _, p := range enabled {
		byEvent, err := pluginHooks(p.path)
		if err != nil {
			note("plugin hooks", err)
		}
		add(wire.ScopePlugin, p.key, byEvent)
	}
	return out
}

func hook(event, source, plugin string, h hookEntry, home string) wire.InventoryHook {
	out := wire.InventoryHook{Event: event, Source: source, Plugin: plugin}
	text := h.Command
	switch {
	case h.Command != "":
		out.Program = commandProgram(h.Command)
	default:
		// A hook that runs no command (a prompt, an HTTP call) is named by its type.
		out.Program = programName(h.Type)
		text = h.Prompt + h.URL
	}
	if text != "" {
		// The home directory is taken out first, so one hook hashes the same on every machine.
		if home != "" {
			text = strings.ReplaceAll(text, home, "~")
		}
		sum := sha256.Sum256([]byte(text))
		out.Hash = hex.EncodeToString(sum[:8])
	}
	return out
}

var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandProgram names the program a hook command starts: the base name of its first word,
// after any leading VAR=value assignments. "" when that word is not plainly a program name.
func commandProgram(cmd string) string {
	rest := strings.TrimSpace(cmd)
	for rest != "" {
		var word string
		if q := rest[0]; q == '"' || q == '\'' {
			end := strings.IndexByte(rest[1:], q)
			if end < 0 {
				return ""
			}
			word, rest = rest[1:1+end], strings.TrimSpace(rest[end+2:])
		} else {
			word, rest, _ = strings.Cut(rest, " ")
			rest = strings.TrimSpace(rest)
		}
		if envAssignRe.MatchString(word) {
			continue
		}
		return programName(word)
	}
	return ""
}

// pluginHooks reads the hooks an installed plugin declares: the file its manifest names, or
// hooks/hooks.json. A manifest path that leaves the plugin's own directory is not followed.
func pluginHooks(installPath string) (map[string][]hookGroup, error) {
	var manifest struct {
		Hooks json.RawMessage `json:"hooks"`
	}
	_ = readJSON(filepath.Join(installPath, ".claude-plugin", "plugin.json"), &manifest)
	rel := filepath.Join("hooks", "hooks.json")
	if len(manifest.Hooks) > 0 {
		var inline struct {
			Hooks map[string][]hookGroup `json:"hooks"`
		}
		var named string
		switch {
		case json.Unmarshal(manifest.Hooks, &named) == nil:
			rel = filepath.FromSlash(named)
		case json.Unmarshal(manifest.Hooks, &inline) == nil && inline.Hooks != nil:
			return inline.Hooks, nil
		}
	}
	path := filepath.Join(installPath, rel)
	if r, err := filepath.Rel(installPath, path); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("hooks path leaves the plugin directory")
	}
	var file struct {
		Hooks map[string][]hookGroup `json:"hooks"`
	}
	if err := readJSON(path, &file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return file.Hooks, nil
}

// connectorPrefix starts the server name of a claude.ai connector in the terminal and the VS
// Code extension, where the session names it "claude.ai <Name>".
const (
	connectorPrefix     = "claude_ai_"
	connectorNamePrefix = "claude.ai "
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sessionConnectors lists the claude.ai connectors loaded in this session, used or not.
//
// Claude Code names a connector two ways, by surface. In the terminal and the VS Code extension
// its server name is "claude_ai_<Name>", so the name is in the transcript. The desktop app names
// it by an opaque id instead, and keeps the id's name and URL in its own per-session file, which
// is the only place the host is found.
func sessionConnectors(ev agent.Event, global globalConfig) ([]wire.InventoryConnector, error) {
	if ev.TranscriptPath == "" {
		return nil, nil
	}
	servers, err := transcript.ParseSessionServers(ev.TranscriptPath)
	if err != nil {
		return nil, err
	}
	// How this install spells its connectors' names, to turn a tool-name spelling back into one.
	spelled := map[string]string{}
	for _, name := range global.EverConnected {
		spelled[transcript.NormalizeServerName(name)] = name
	}
	var out []wire.InventoryConnector
	var ids []string
	for _, s := range servers.Names {
		switch {
		case strings.HasPrefix(s, connectorPrefix):
			name := servers.Display[s]
			if name == "" {
				name = spelled[s]
			}
			if name == "" {
				name = strings.ReplaceAll(s, "_", " ")
			}
			name = strings.TrimPrefix(strings.TrimPrefix(name, connectorNamePrefix), "claude ai ")
			out = append(out, wire.InventoryConnector{Name: name, ID: s, Auth: connectorAuth(servers.Tools[s])})
		case uuidRe.MatchString(s):
			ids = append(ids, s)
		}
	}
	if len(ids) > 0 {
		known := desktopConnectors(ids)
		for _, id := range ids {
			// An id the desktop app's files do not name is left out: an opaque id says nothing.
			if c, ok := known[id]; ok {
				out = append(out, wire.InventoryConnector{Name: c.Name, ID: id, Host: hostOf(c.URL), Auth: connectorAuth(servers.Tools[id])})
			}
		}
	}
	return out, nil
}

// signInTools are the only tools a claude.ai connector loads while the developer has not signed
// in to it: they start the sign-in and do nothing else.
var signInTools = set("authenticate", "complete_authentication")

// connectorAuth says whether the developer has signed in to a connector, from the tools the
// session loaded for it. One real tool is enough for "signed in", so a connector signed in to
// part-way through a session reads as signed in. Measured 2026-11-13 on 38 sessions of one
// install: the terminal listed 27 connectors, 17 of them with exactly the two sign-in tools; the
// desktop app, the VS Code extension and headless runs listed only the 10 signed-in ones.
func connectorAuth(tools []string) string {
	auth := ""
	for _, tool := range tools {
		if !signInTools[tool] {
			return wire.ConnectorSignedIn
		}
		auth = wire.ConnectorNotSignedIn
	}
	return auth
}

// desktopSessionFiles bounds how many of the desktop app's session files are opened. The list of
// connectors is per account, so the newest file nearly always names every id.
const desktopSessionFiles = 5

type desktopConnector struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// desktopConnectors resolves connector ids through the Claude desktop app's session files,
// newest first, stopping once every id is named.
func desktopConnectors(ids []string) map[string]desktopConnector {
	base, err := userConfigDir()
	if err != nil || base == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(base, "Claude", "claude-code-sessions", "*", "*", "local_*.json"))
	type aged struct {
		path string
		mod  int64
	}
	var byAge []aged
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			byAge = append(byAge, aged{f, fi.ModTime().UnixNano()})
		}
	}
	sort.Slice(byAge, func(i, j int) bool { return byAge[i].mod > byAge[j].mod })
	want := set(ids...)
	known := map[string]desktopConnector{}
	for i, f := range byAge {
		if i == desktopSessionFiles || len(known) == len(want) {
			break
		}
		var session struct {
			Connectors []desktopConnector `json:"remoteMcpServersConfig"`
		}
		if readJSON(f.path, &session) != nil {
			continue
		}
		for _, c := range session.Connectors {
			if want[c.UUID] && c.Name != "" {
				if _, seen := known[c.UUID]; !seen {
					known[c.UUID] = c
				}
			}
		}
	}
	return known
}
