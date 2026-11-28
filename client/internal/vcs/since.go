package vcs

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/leotrace-hq/leoprevent-plugin/limits"
	"github.com/leotrace-hq/leoprevent-plugin/pathgate"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

func ChangesSince(root string,
	rev string) []wire.ChangedFile {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return nil
	}
	nameStatus, err := git(root, "diff", "--name-status", "-M", rev, "--")
	if err != nil {
		return nil
	}
	var out []wire.ChangedFile
	for _, tf := range trackedFiles(nameStatus) {
		p := tf.path
		if skipSince(p) {
			continue
		}
		if tf.deleted {
			if removed, ok := deletedContent(root, rev, p); ok {
				out = append(out, wire.ChangedFile{Path: p, RemovedText: removed, Deleted: true})
			}
			continue
		}
		args := []string{"diff", "-M", rev, "--"}
		if tf.oldPath != "" {
			args = append(args, tf.oldPath)
		}
		diff, err := git(root, append(args, p)...)
		if err != nil {
			continue
		}
		removed, removedAt := removedLines(diff, limits.MaxChangedFileBytes)
		out = append(out, wire.ChangedFile{
			Path:        p,
			AddedText:   addedLines(diff),
			AddedLines:  addedLineNumbers(diff),
			RemovedText: removed,
			RemovedAt:   removedAt,
		})
	}
	untracked, _ := git(root, "ls-files", "--others", "--exclude-standard", "--", excludeNodeModules)
	for _, p := range strings.Split(strings.TrimSpace(untracked), "\n") {
		if p = strings.TrimSpace(p); p == "" || skipSince(p) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, p))
		if err != nil || len(body) > limits.MaxChangedFileBytes {
			continue
		}
		text := string(body)
		n := strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
		lines := make([]int, n)
		for i := range lines {
			lines[i] = i + 1
		}
		out = append(out, wire.ChangedFile{Path: p, AddedText: text, AddedLines: lines})
	}
	return out
}

func skipSince(p string) bool {
	return pathgate.IsSecretPath(p) || strings.Contains("/"+p, "/node_modules/")
}
