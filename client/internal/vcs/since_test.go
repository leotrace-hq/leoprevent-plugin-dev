package vcs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangesSinceListsEveryEditSinceTheSweptCommit(t *testing.T) {
	// Arrange
	dir, _ := initRepo(t)
	for name, body := range map[string]string{"store.ts": "a\nb\n", "guard.ts": "guard()\n", ".env": "KEY=1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "swept")
	swept := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(dir, "store.ts"), []byte("a\ncheck()\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "rm", "-q", "guard.ts")
	gitRun(t, dir, "commit", "-q", "-am", "fix")
	if err := os.WriteFile(filepath.Join(dir, "paths.ts"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KEY=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	got := ChangesSince(dir, swept)

	// Assert
	byPath := map[string]int{}
	for i, c := range got {
		byPath[c.Path] = i
	}
	if len(got) != 3 {
		t.Fatalf("changes = %+v", got)
	}
	if c := got[byPath["store.ts"]]; c.AddedText != "check()\n" || len(c.AddedLines) != 1 || c.AddedLines[0] != 2 || c.FullContent != "" {
		t.Fatalf("store.ts = %+v", c)
	}
	if c := got[byPath["guard.ts"]]; !c.Deleted || c.RemovedText != "guard()\n" {
		t.Fatalf("guard.ts = %+v", c)
	}
	if c := got[byPath["paths.ts"]]; c.AddedText != "one\ntwo\n" || len(c.AddedLines) != 2 {
		t.Fatalf("paths.ts = %+v", c)
	}
	if ChangesSince(dir, "") != nil || ChangesSince(dir, "--output=x") != nil {
		t.Fatal("an empty or option-shaped revision lists nothing")
	}
}
