package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// Only something shaped like a host leaves a URL. A malformed one can otherwise put part of a
// credential in the host, and a value that is no URL at all would be sent whole.
func TestHostOfSendsOnlyAHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://mcp.linear.app/sse?x=1":        "mcp.linear.app",
		"https://user:pw@Host.Example:8443/mcp": "host.example:8443",
		"http://localhost:3000/mcp":             "localhost:3000",
		"http://127.0.0.1:8787":                 "127.0.0.1:8787",
		"http://[::1]:9000/x":                   "[::1]:9000",
		"https://dG9r/ZW4=@host.example/mcp":    "",
		"https://user:12/34@host.example/mcp":   "",
		"sk-secret-value":                       "",
		"":                                      "",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A program path may hold spaces. It is never split into a folder name and the rest.
func TestLauncherIsNeverAFolderName(t *testing.T) {
	cases := []struct {
		name     string
		cfg      mcpServerConfig
		launcher string
		pkg      string
	}{
		{"path with a space and an args list", mcpServerConfig{Command: `/Users/Jane Doe/bin/server`, Args: []string{"--port", "1"}}, "server", ""},
		{"one string that starts with a path", mcpServerConfig{Command: `/Users/Jane Doe/bin/server --port 1`}, "", ""},
		{"one string, plain runner", mcpServerConfig{Command: "npx -y @playwright/mcp@latest"}, "npx", "@playwright/mcp@latest"},
		{"runner with an args list", mcpServerConfig{Command: "npx", Args: []string{"-y", "@playwright/mcp@latest"}}, "npx", "@playwright/mcp@latest"},
	}
	for _, c := range cases {
		got := mcpServer("s", "user", c.cfg)
		if got.Launcher != c.launcher || got.Package != c.pkg {
			t.Errorf("%s: launcher=%q package=%q, want %q and %q", c.name, got.Launcher, got.Package, c.launcher, c.pkg)
		}
	}
}

// A linked subagent file is hashed, but its text is not read for a name: the link can point at
// any file on the machine. A name with an underscore is a name.
func TestLinkedInstructionIsNotReadForAName(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(secret, []byte("---\nname: prod-db-password-holder\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(agents, "linked.md")); err != nil {
		t.Skip("no symlinks here")
	}
	if err := os.WriteFile(filepath.Join(agents, "my_reviewer.md"), []byte("---\nname: code_reviewer\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var c collector
	c.markdownIn(agents, "project", "", false, true)
	names := map[string]bool{}
	for _, i := range c.out {
		names[i.Name] = true
	}
	if names["prod-db-password-holder"] {
		t.Error("a linked file's name: line was sent")
	}
	if !names["linked"] || !names["code_reviewer"] {
		t.Errorf("names = %v, want the link under its own file name and the underscore name kept", names)
	}
}

// The number of files opened is bounded, not only the number kept.
func TestInstructionReadsAreBounded(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxInstructionReads+50; i++ {
		// A name the name check refuses, so none of these counts towards what is kept.
		if err := os.WriteFile(filepath.Join(dir, "bad name!"+string(rune('a'+i%26))+filepath.Base(t.TempDir())+".md"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var c collector
	c.markdownIn(dir, "project", "", false, false)
	if c.read != maxInstructionReads {
		t.Errorf("opened %d files, want the bound %d", c.read, maxInstructionReads)
	}
}
