package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// This file is the part of Claude Code's agent inventory (LEO-822) that is read from Markdown
// files rather than settings: skills, custom slash commands and custom subagents. Each is text
// the agent follows with the developer's rights.
//
// ⚠️ THE TEXT IS READ TO BE HASHED AND GOES NO FURTHER. What leaves the machine for one of these
// is its name, where it is installed and a hash (wire.InventoryInstruction). A subagent's `name:`
// line is the one value read out of a file, because that line, not the file's name, is what
// Claude Code knows the subagent by.
//
// Where Claude Code looks, from its docs (code.claude.com/docs, skills, sub-agents and plugins
// reference, read 2026-11-13): a skill is a folder holding SKILL.md, named by the folder; a
// command is a .md file, and a plugin's subfolders become part of its name; a subagent is a .md
// file found at any depth. Each kind is loaded from the managed settings directory, the user's
// config directory, the project and every enabled plugin. Skills the signed-in claude.ai account
// brings are synced into the user's skills folder.

const (
	// maxInstructionBytes bounds one instruction file. A skill is a page or two of text.
	maxInstructionBytes = 1 << 20
	// maxInstructionFiles bounds what is collected per kind, and maxInstructionDepth how deep a
	// commands or agents folder is walked.
	maxInstructionFiles = 400
	// maxInstructionReads bounds how many files one kind opens in all, kept or not.
	maxInstructionReads = 2000
	maxInstructionDepth = 4
)

// instructionNameRe is what a skill's, command's or subagent's name may look like to be sent.
// These are file and folder names a developer chose: one that is not plainly a name is dropped
// rather than cleaned.
var instructionNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:+_-]{0,99}$`)

// isSymlink reports whether path itself is a symbolic link.
func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// readInstruction reads one instruction file, following symlinks: a dotfile manager links these
// into place. False for a missing, oversized or irregular file.
func readInstruction(path string) ([]byte, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxInstructionBytes {
		return nil, false
	}
	data, err := os.ReadFile(path)
	return data, err == nil
}

// frontmatterName returns the `name:` value of a Markdown file's leading YAML block, or "".
func frontmatterName(data []byte) string {
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) == 0 || strings.TrimSpace(string(lines[0])) != "---" {
		return ""
	}
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(string(raw))
		if line == "---" {
			break
		}
		if value, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

// collector gathers the instructions of one kind, once each per place it is installed.
type collector struct {
	out  []wire.InventoryInstruction
	seen map[string]bool
	// read counts the files opened, kept or not. maxInstructionFiles bounds what is sent;
	// this bounds the work, so a folder of thousands of files cannot hold the hook.
	read int
}

// spent reports whether this collector has opened as many files as it may.
func (c *collector) spent() bool { return c.read >= maxInstructionReads }

func (c *collector) add(name, source, plugin string, data []byte) {
	key := source + "\x00" + plugin + "\x00" + name
	if len(c.out) >= maxInstructionFiles || !instructionNameRe.MatchString(name) || c.seen[key] {
		return
	}
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	c.seen[key] = true
	sum := sha256.Sum256(data)
	c.out = append(c.out, wire.InventoryInstruction{Name: name, Source: source, Plugin: plugin, Hash: hex.EncodeToString(sum[:8])})
}

// skillsIn adds every skill in dir: a folder holding SKILL.md, named by the folder.
func (c *collector) skillsIn(dir, source, plugin string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		// A dot folder is Claude Code's own bookkeeping (its trash), never a skill.
		if c.spent() {
			return
		}
		if name := e.Name(); !strings.HasPrefix(name, ".") {
			c.read++
			if data, ok := readInstruction(filepath.Join(dir, name, "SKILL.md")); ok {
				c.add(name, source, plugin, data)
			}
		}
	}
}

// markdownIn adds every .md file under dir. byPath names a file by its path under dir, with
// subfolders joined by a colon ("db/migrate.md" is "db:migrate"), which is how a plugin's
// commands and subagents are named. byFrontmatter lets a file's `name:` line replace its own
// name, which is how a subagent is named.
func (c *collector) markdownIn(dir, source, plugin string, byPath, byFrontmatter bool) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable folder costs that folder
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || strings.Count(rel, string(filepath.Separator)) >= maxInstructionDepth) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if c.spent() {
			return filepath.SkipAll
		}
		c.read++
		if data, ok := readInstruction(path); ok {
			// A linked file is hashed like any other, but its text is never read for a name:
			// a link can point at any file on the machine, and a `name:` line of that file
			// would leave with the snapshot.
			c.add(markdownName(rel, data, byPath, byFrontmatter && !isSymlink(path)), source, plugin, data)
		}
		return nil
	})
}

func markdownName(rel string, data []byte, byPath, byFrontmatter bool) string {
	parts := strings.Split(strings.TrimSuffix(filepath.ToSlash(rel), ".md"), "/")
	if byFrontmatter {
		if name := frontmatterName(data); name != "" {
			parts[len(parts)-1] = name
		}
	}
	if !byPath {
		return parts[len(parts)-1]
	}
	return strings.Join(parts, ":")
}

// instructions lists the skills, custom slash commands and custom subagents that apply to this
// project.
func instructions(roots []string, enabled []enabledPlugin) (skills, commands, subagents []wire.InventoryInstruction) {
	var s, c, a collector
	scan := func(base, source string) {
		s.skillsIn(filepath.Join(base, "skills"), source, "")
		// Claude Code's docs name a plugin's command by its subfolders and are silent on the
		// user's and the project's; the same naming is used for all three.
		c.markdownIn(filepath.Join(base, "commands"), source, "", true, false)
		a.markdownIn(filepath.Join(base, "agents"), source, "", false, true)
	}
	for _, f := range managedSettingsFiles() {
		if strings.HasSuffix(f, ".json") {
			scan(filepath.Join(filepath.Dir(f), ".claude"), wire.ScopeManaged)
		}
	}
	if dir := configDir(); dir != "" {
		scan(dir, wire.ScopeUser)
		// The claude.ai account's skills, synced into one folder per account. The folder's name
		// is an account id and is not read into anything.
		synced := filepath.Join(dir, "skills", "synced")
		if accounts, err := os.ReadDir(synced); err == nil {
			for _, acct := range accounts {
				if !strings.HasPrefix(acct.Name(), ".") {
					s.skillsIn(filepath.Join(synced, acct.Name()), wire.ScopeAccount, "")
				}
			}
		}
	}
	for _, r := range roots {
		scan(filepath.Join(r, ".claude"), wire.ScopeProject)
	}
	for _, p := range enabled {
		pluginInstructions(p, &s, &c, &a)
	}
	return s.out, c.out, a.out
}

// insidePlugin resolves a path a plugin's manifest names, and refuses one that leaves the
// plugin's own directory.
func insidePlugin(installPath, rel string) (string, bool) {
	path := filepath.Join(installPath, filepath.FromSlash(rel))
	r, err := filepath.Rel(installPath, path)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return path, true
}

// manifestPaths reads a manifest field that is one path or a list of paths.
func manifestPaths(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

// pluginInstructions adds what one enabled plugin brings: its skills/, commands/ and agents/
// folders, and whatever its manifest names instead of or beside them.
func pluginInstructions(p enabledPlugin, skills, commands, subagents *collector) {
	var manifest struct {
		Skills   json.RawMessage `json:"skills"`
		Commands json.RawMessage `json:"commands"`
		Agents   json.RawMessage `json:"agents"`
	}
	_ = readJSON(filepath.Join(p.path, ".claude-plugin", "plugin.json"), &manifest)

	// Skills: the manifest's paths are scanned as well as skills/. Each is a folder of skill
	// folders, or one skill's own folder.
	skills.skillsIn(filepath.Join(p.path, "skills"), wire.ScopePlugin, p.key)
	for _, rel := range manifestPaths(manifest.Skills) {
		dir, ok := insidePlugin(p.path, rel)
		if !ok {
			continue
		}
		if data, isSkill := readInstruction(filepath.Join(dir, "SKILL.md")); isSkill {
			name := frontmatterName(data)
			if name == "" {
				name = filepath.Base(dir)
			}
			skills.add(name, wire.ScopePlugin, p.key, data)
			continue
		}
		skills.skillsIn(dir, wire.ScopePlugin, p.key)
	}

	// Commands: the manifest's entry replaces commands/. It is a path, a list of paths, or a map
	// of command name to a source file or to the command's text.
	switch {
	case len(manifest.Commands) == 0:
		commands.markdownIn(filepath.Join(p.path, "commands"), wire.ScopePlugin, p.key, true, false)
	default:
		var named map[string]struct {
			Source  string `json:"source"`
			Content string `json:"content"`
		}
		if json.Unmarshal(manifest.Commands, &named) == nil {
			for name, cmd := range named {
				data := []byte(cmd.Content)
				if path, ok := insidePlugin(p.path, cmd.Source); cmd.Source != "" && ok {
					if file, read := readInstruction(path); read {
						data = file
					}
				}
				commands.add(name, wire.ScopePlugin, p.key, data)
			}
			break
		}
		for _, rel := range manifestPaths(manifest.Commands) {
			addMarkdownPath(commands, p, rel, true, false)
		}
	}

	// Subagents: the manifest's entry replaces agents/ and lists .md files.
	if len(manifest.Agents) == 0 {
		subagents.markdownIn(filepath.Join(p.path, "agents"), wire.ScopePlugin, p.key, true, true)
		return
	}
	for _, rel := range manifestPaths(manifest.Agents) {
		addMarkdownPath(subagents, p, rel, true, true)
	}
}

// addMarkdownPath adds what one manifest path names: a .md file, or a folder of them.
func addMarkdownPath(c *collector, p enabledPlugin, rel string, byPath, byFrontmatter bool) {
	path, ok := insidePlugin(p.path, rel)
	if !ok {
		return
	}
	if strings.HasSuffix(path, ".md") {
		if data, read := readInstruction(path); read {
			c.add(markdownName(filepath.Base(path), data, byPath, byFrontmatter), wire.ScopePlugin, p.key, data)
		}
		return
	}
	c.markdownIn(path, wire.ScopePlugin, p.key, byPath, byFrontmatter)
}
