package sweep

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leotrace-hq/leoprevent-plugin/limits"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

func write(t *testing.T,
	root string,
	rel string,
	body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadSendsOnlySafeTextInsideTheRepository(t *testing.T) {
	// Arrange
	root := t.TempDir()
	outside := t.TempDir()
	write(t, root, "src/app.py", "print(1)\n")
	write(t, root, ".env", "KEY=1\n")
	write(t, root, "bin.dat", "a\x00b")
	write(t, root, "huge.py", strings.Repeat("x", limits.MaxChangedFileBytes+1))
	write(t, outside, "secret.py", "x\n")
	if err := os.Symlink(filepath.Join(outside, "secret.py"), filepath.Join(root, "link.py")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"src/app.py":        "",
		"/src/app.py":       "",
		".env":              SkipSecret,
		"bin.dat":           SkipBinary,
		"huge.py":           SkipTooLarge,
		"link.py":           SkipSymlink,
		"linkdir/secret.py": SkipSymlink,
		"../outside.py":     SkipUnsafePath,
		"gone.py":           SkipMissing,
	}

	for path, want := range cases {
		// Act
		content, skip := Read(root, path)

		// Assert
		if skip != want {
			t.Fatalf("%s: skip = %q, want %q", path, skip, want)
		}
		if want == "" && content != "print(1)\n" {
			t.Fatalf("%s: content = %q", path, content)
		}
		if want != "" && content != "" {
			t.Fatalf("%s: a skipped file must carry no content", path)
		}
	}
}

func TestBatchesRespectBytesFilesAndIssues(t *testing.T) {
	// Arrange
	files := []File{
		{Path: "a", Issues: 1, Content: "1234"},
		{Path: "b", Issues: 1, Content: "1234"},
		{Path: "c", Issues: 5, Content: "1"},
		{Path: "d", Issues: 1, Content: "1"},
		{Path: "e", Issues: 1, Content: "1"},
	}

	// Act
	got := Batches(files, 8, 3, 6)

	// Assert
	var shape []string
	for _, b := range got {
		var names []string
		for _, f := range b {
			names = append(names, f.Path)
		}
		shape = append(shape, strings.Join(names, ""))
	}
	if strings.Join(shape, "|") != "ab|cd|e" {
		t.Fatalf("batches = %v", shape)
	}
}

func TestBatchesKeepAnOversizedFileAlone(t *testing.T) {
	// Arrange
	files := []File{{Path: "a", Content: "1"}, {Path: "big", Content: "123456789"}, {Path: "b", Content: "1"}}

	// Act
	got := Batches(files, 4, 10, 10)

	// Assert
	if len(got) != 3 || got[1][0].Path != "big" {
		t.Fatalf("batches = %+v", got)
	}
}

func TestStateRoundTripsAndTakesABatchOnce(t *testing.T) {
	// Arrange
	defer SetUserConfigDirForTest(t.TempDir())()
	st := State{Tool: "aikido", Repo: "github.com/acme/api", Pending: []Pending{
		{ReviewID: "r1", Findings: []wire.Finding{{Rule: "aikido-fp:x", Location: "a.py:1"}}},
		{ReviewID: "r2"},
	}}

	// Act
	if err := Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := Load("aikido", "github.com/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := Load("aikido", "github.com/acme/web")
	snyk, _ := Load("snyk", "github.com/acme/api")
	p, ok := got.Take("r1")
	_, again := got.Take("r1")

	// Assert
	if !ok || again || p.Findings[0].Location != "a.py:1" || got.Repo != st.Repo || len(got.Pending) != 1 || len(other.Pending) != 0 || len(snyk.Pending) != 0 {
		t.Fatalf("take = %+v %v %v, state = %+v", p, ok, again, got)
	}
}

func TestLoadWithNoSavedSweepIsEmpty(t *testing.T) {
	// Arrange
	defer SetUserConfigDirForTest(t.TempDir())()

	// Act
	st, err := Load("snyk", "github.com/acme/api")

	// Assert
	if err != nil || len(st.Pending) != 0 {
		t.Fatalf("st = %+v err = %v", st, err)
	}
}

func TestDropExpiredKeepsRecentOpenFindings(t *testing.T) {
	// Arrange
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	st := State{Open: []Open{
		{ReviewID: "fresh", At: now.Add(-time.Hour), Findings: []wire.Finding{{Location: "a.py:1"}}},
		{ReviewID: "stale", At: now.Add(-OpenTTL - time.Hour), Findings: []wire.Finding{{Location: "a.py:1"}}},
		{ReviewID: "empty", At: now},
	}}

	// Act
	st.DropExpired(now)

	// Assert
	if len(st.Open) != 1 || st.Open[0].ReviewID != "fresh" {
		t.Fatalf("open = %+v", st.Open)
	}
}

func TestChangedSendsOnlyFilesThatDifferFromTheSweptCommit(t *testing.T) {
	// Arrange
	committed := map[string]string{"a.py": "old a", "b.py": "same b", "gone.py": "old gone"}
	current := map[string]string{"a.py": "new a", "b.py": "same b"}
	before := func(_, rev, path string) (string, bool) {
		v, ok := committed[path]
		return v, ok && rev == "abc"
	}
	read := func(_, path string) (string, string) {
		if v, ok := current[path]; ok {
			return v, ""
		}
		return "", SkipMissing
	}
	o := Open{ReviewID: "r1", Commit: "abc", Findings: []wire.Finding{
		{Rule: "x", Location: "a.py:3"}, {Rule: "y", Location: "a.py:9"}, {Rule: "x", Location: "b.py:1"}, {Rule: "x", Location: "gone.py:1"},
	}}

	// Act
	touched, untouched, b, a := Changed("/repo", o, nil, before, read)

	// Assert
	if len(touched) != 2 || touched[0].Location != "a.py:3" || touched[1].Location != "a.py:9" {
		t.Fatalf("touched = %+v", touched)
	}
	if len(untouched) != 2 {
		t.Fatalf("an unchanged or deleted file is never sent for re-checking: %+v", untouched)
	}
	if len(b) != 1 || b[0].Path != "a.py" || b[0].FullContent != "old a" || len(a) != 1 || a[0].FullContent != "new a" {
		t.Fatalf("before = %+v after = %+v", b, a)
	}
}

func TestChangedRechecksAFindingWhoseFixLandedInAnotherFile(t *testing.T) {
	// Arrange
	committed := map[string]string{"app/route.ts": "route", "lib/store.ts": "old store"}
	current := map[string]string{"app/route.ts": "route", "lib/store.ts": "new store", "secrets/.env": "KEY=1"}
	before := func(_, rev, path string) (string, bool) {
		v, ok := committed[path]
		return v, ok && rev == "abc"
	}
	read := func(_, path string) (string, string) {
		if path == "secrets/.env" {
			return "", SkipSecret
		}
		if v, ok := current[path]; ok {
			return v, ""
		}
		return "", SkipMissing
	}
	others := []wire.ChangedFile{
		{Path: "lib/store.ts", AddedText: "check()\n", AddedLines: []int{4}},
		{Path: "secrets/.env", AddedText: "KEY=1\n", AddedLines: []int{1}},
		{Path: "app/route.ts", AddedText: "x\n", AddedLines: []int{1}},
		{Path: "old/guard.ts", RemovedText: "guard()\n", Deleted: true},
	}
	o := Open{ReviewID: "r1", Commit: "abc", Findings: []wire.Finding{{Rule: "x", Location: "app/route.ts:21"}}}

	// Act
	touched, untouched, b, a := Changed("/repo", o, others, before, read)

	// Assert
	if len(touched) != 1 || len(untouched) != 0 {
		t.Fatalf("touched = %+v untouched = %+v", touched, untouched)
	}
	if len(b) != 1 || b[0].Path != "app/route.ts" || b[0].FullContent != "route" {
		t.Fatalf("before = %+v", b)
	}
	paths := []string{}
	for _, c := range a {
		paths = append(paths, c.Path)
	}
	if strings.Join(paths, ",") != "app/route.ts,lib/store.ts,old/guard.ts" || a[0].FullContent != "route" || a[1].FullContent != "" {
		t.Fatalf("after = %+v", a)
	}
}

func TestChangedSendsNothingWhenNoFileChanged(t *testing.T) {
	// Arrange
	before := func(_, _, _ string) (string, bool) { return "same", true }
	read := func(_, _ string) (string, string) { return "same", "" }
	o := Open{ReviewID: "r1", Commit: "abc", Findings: []wire.Finding{{Rule: "x", Location: "a.ts:1"}}}

	// Act
	touched, untouched, b, a := Changed("/repo", o, nil, before, read)

	// Assert
	if len(touched) != 0 || len(untouched) != 1 || len(b) != 0 || len(a) != 0 {
		t.Fatalf("touched = %+v before = %+v after = %+v", touched, b, a)
	}
}

func TestPageTakesFindingsUpToTheLimitInPlanOrder(
	t *testing.T,
) {
	// Arrange
	files := []wire.SweepFile{{Path: "a", Issues: 4}, {Path: "b", Issues: 5}, {Path: "c", Issues: 2}, {Path: "d", Issues: 1}}

	// Act
	page, rest := Page(files, nil, 10)

	// Assert
	if len(page) != 2 || page[0].Path != "a" || page[1].Path != "b" {
		t.Fatalf("page %+v", page)
	}
	if len(rest) != 2 || rest[0].Path != "c" || rest[1].Path != "d" {
		t.Fatalf("rest %+v", rest)
	}
}

func TestPageWithNoLimitTakesEveryUnsweptFile(
	t *testing.T,
) {
	// Arrange
	files := []wire.SweepFile{{Path: "a", Issues: 40}, {Path: "b", Issues: 30}, {Path: "c", Issues: 2}}

	// Act
	page, rest := Page(files, []string{"b"}, math.MaxInt)

	// Assert
	if len(page) != 2 || page[0].Path != "a" || page[1].Path != "c" {
		t.Fatalf("page %+v", page)
	}
	if len(rest) != 0 {
		t.Fatalf("rest %+v", rest)
	}
}

func TestPageSkipsSweptFilesAndTakesAnOversizedFileAlone(
	t *testing.T,
) {
	// Arrange
	files := []wire.SweepFile{{Path: "a", Issues: 4}, {Path: "b", Issues: 30}, {Path: "c", Issues: 2}}

	// Act
	page, rest := Page(files, []string{"a"}, 10)

	// Assert
	if len(page) != 1 || page[0].Path != "b" {
		t.Fatalf("page %+v", page)
	}
	if len(rest) != 1 || rest[0].Path != "c" {
		t.Fatalf("rest %+v", rest)
	}
}

func TestReadExceptNeverReadsAnExcludedPath(t *testing.T) {
	// Arrange
	root := t.TempDir()
	write(t, root, "prototypes/demo.py", "import os\n")
	write(t, root, "app.py", "import os\n")
	read := ReadExcept([]string{"prototypes/**"})

	// Act
	excluded, skip := read(root, "prototypes/demo.py")
	kept, keptSkip := read(root, "app.py")

	// Assert
	if excluded != "" || skip != SkipExcluded {
		t.Fatalf("an excluded file is skipped unread: %q %q", excluded, skip)
	}
	if kept != "import os\n" || keptSkip != "" {
		t.Fatalf("any other file is read as before: %q %q", kept, keptSkip)
	}
}

func TestGuardContextSendsOnlyTheNamedFilesItMayRead(t *testing.T) {
	// Arrange
	root := t.TempDir()
	write(t, root, "web/lib/scim/http.ts", "export function scim() {}\n")
	write(t, root, "web/app/route.ts", "scim()\n")
	write(t, root, ".env", "TOKEN=x\n")
	write(t, root, "vendor/guard.ts", "export const g = 1\n")
	tracked := []string{"web/lib/scim/http.ts", "web/app/route.ts", ".env", "vendor/guard.ts", "a/dup.ts", "b/dup.ts"}
	want := guardsFor("lib/scim/http.ts", "web/app/route.ts", ".env", "vendor/guard.ts", "dup.ts", "missing.ts", "../outside.ts")

	// Act
	got := GuardContext(root, want, map[string]bool{"web/app/route.ts": true}, tracked, ReadExcept([]string{"vendor/**"}), limits.MaxReviewBytes)

	// Assert
	if len(got) != 1 || got[0].Path != "web/lib/scim/http.ts" || got[0].Content != "export function scim() {}\n" {
		t.Fatalf("context = %+v", got)
	}
}

func TestGuardContextStopsAtTheRequestLimits(t *testing.T) {
	// Arrange
	root := t.TempDir()
	var tracked []string
	var want []wire.SweepGuard
	for i := 0; i < wire.MaxSweepContextFiles+3; i++ {
		p := "g" + strings.Repeat("x", i) + ".ts"
		write(t, root, p, "ok\n")
		tracked, want = append(tracked, p), append(want, wire.SweepGuard{Path: p})
	}
	write(t, root, "big.ts", strings.Repeat("a", 64))
	tracked = append(tracked, "big.ts")

	// Act
	capped := GuardContext(root, want, nil, tracked, Read, limits.MaxReviewBytes)
	tooBig := GuardContext(root, guardsFor("big.ts"), nil, tracked, Read, 32)

	// Assert
	if len(capped) != wire.MaxSweepContextFiles {
		t.Fatalf("context files = %d, want %d", len(capped), wire.MaxSweepContextFiles)
	}
	if len(tooBig) != 0 {
		t.Fatalf("a file over the byte budget must not be sent, got %+v", tooBig)
	}
}

func TestWithGuardsFollowsAProtectionAcrossFilesBeforeAskingForAFinalVerdict(t *testing.T) {
	// Arrange
	answers := []wire.SweepResponse{
		{Unjudged: 1, NeedsContext: []string{"lib/orders.ts"}},
		{Disputed: 1},
	}
	var sent []wire.SweepRequest
	send := func(r wire.SweepRequest) (wire.SweepResponse, error) {
		sent = append(sent, r)
		next := answers[0]
		answers = answers[1:]
		return next, nil
	}
	fetch := func(need []wire.SweepGuard, _ []wire.ContextFile) []wire.ContextFile {
		out := []wire.ContextFile{}
		for _, g := range need {
			out = append(out, wire.ContextFile{Path: g.Path, Content: "x"})
		}
		return out
	}

	// Act
	resp, err := WithGuards(wire.SweepResponse{Unjudged: 1, NeedsContext: []string{"app/orders/actions.ts"}}, wire.SweepRequest{}, send, fetch)

	// Assert
	if err != nil || resp.Disputed != 1 || resp.Unjudged != 0 {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	if len(sent) != 2 || sent[0].ContextSent || len(sent[0].Context) != 1 {
		t.Fatalf("the first round must not be final while a new file was added, got %+v", sent)
	}
	if len(sent[1].Context) != 2 || sent[1].Context[1].Path != "lib/orders.ts" {
		t.Fatalf("each round must carry every file sent so far, got %+v", sent[1].Context)
	}
}

func TestWithGuardsIsFinalWhenNothingNewCanBeRead(t *testing.T) {
	// Arrange
	var sent []wire.SweepRequest
	send := func(r wire.SweepRequest) (wire.SweepResponse, error) {
		sent = append(sent, r)
		return wire.SweepResponse{Real: 1}, nil
	}
	fetch := func([]wire.SweepGuard, []wire.ContextFile) []wire.ContextFile { return nil }

	// Act
	resp, _ := WithGuards(wire.SweepResponse{Unjudged: 1, NeedsContext: []string{".env"}}, wire.SweepRequest{}, send, fetch)

	// Assert
	if len(sent) != 1 || !sent[0].ContextSent || resp.Real != 1 {
		t.Fatalf("a file that cannot be read ends the rounds with a final request, got %+v", sent)
	}
}

func TestWithGuardsStopsAfterTheLastRound(t *testing.T) {
	// Arrange
	n := 0
	var sent []wire.SweepRequest
	send := func(r wire.SweepRequest) (wire.SweepResponse, error) {
		sent = append(sent, r)
		n++
		if r.ContextSent {
			return wire.SweepResponse{Real: 1}, nil
		}
		return wire.SweepResponse{Unjudged: 1, NeedsContext: []string{"f" + strings.Repeat("x", n)}}, nil
	}
	fetch := func(need []wire.SweepGuard, _ []wire.ContextFile) []wire.ContextFile {
		return []wire.ContextFile{{Path: need[0].Path, Content: "x"}}
	}

	// Act
	resp, _ := WithGuards(wire.SweepResponse{NeedsContext: []string{"f"}}, wire.SweepRequest{}, send, fetch)

	// Assert
	if len(sent) != MaxContextRounds || !sent[len(sent)-1].ContextSent || resp.Real != 1 {
		t.Fatalf("rounds = %d, last final = %v", len(sent), sent[len(sent)-1].ContextSent)
	}
}

func guardsFor(paths ...string) []wire.SweepGuard {
	out := make([]wire.SweepGuard, len(paths))
	for i, p := range paths {
		out[i] = wire.SweepGuard{Path: p}
	}
	return out
}

func TestGuardContextFindsAProtectionNamedByItsModuleThroughItsSymbol(t *testing.T) {
	// Arrange
	root := t.TempDir()
	write(t, root, "packages/auth/src/emailcodes.ts", "export async function verifyCode(email: string) {}\n")
	write(t, root, "packages/auth/src/emailcodes.test.ts", "export async function verifyCode() {}\n")
	write(t, root, "internal/reviewstore/mongoload.go", "package reviewstore\n\nfunc LoadFromMongo() {}\n")
	write(t, root, "web/app/caller.ts", "verifyCode(email)\n")
	tracked := []string{"packages/auth/src/emailcodes.ts", "packages/auth/src/emailcodes.test.ts", "internal/reviewstore/mongoload.go", "web/app/caller.ts"}
	want := []wire.SweepGuard{
		{Path: "@leoprevent/auth", Symbol: "verifyCode"},
		{Path: "leoprevent/internal/reviewstore", Symbol: "LoadFromMongo"},
		{Path: "server/internal/sca/validate.go", Symbol: "Validate"},
	}

	// Act
	got := GuardContext(root, want, nil, tracked, Read, limits.MaxReviewBytes)

	// Assert
	paths := []string{}
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	if strings.Join(paths, ",") != "packages/auth/src/emailcodes.ts,internal/reviewstore/mongoload.go" {
		t.Fatalf("context = %v", paths)
	}
}

func TestGuardContextMatchesModuleHintsByWholePathSegment(t *testing.T) {
	// Arrange
	root := t.TempDir()
	write(t, root, "server/internal/sca/check.go", "package sca\n\nfunc Validate(req Request) error { return nil }\n")
	write(t, root, "server/internal/reposcan/scan.go", "package reposcan\n\nfunc Validate(out Output) {}\n")
	write(t, root, "server/internal/migrate/migrate.go", "package migrate\n\nfunc Validate(rec Record) error { return nil }\n")
	tracked := []string{"server/internal/sca/check.go", "server/internal/reposcan/scan.go", "server/internal/migrate/migrate.go"}
	want := []wire.SweepGuard{{Path: "server/internal/sca/validate.go", Symbol: "Validate"}}

	// Act
	got := GuardContext(root, want, nil, tracked, Read, limits.MaxReviewBytes)

	// Assert
	if len(got) != 1 || got[0].Path != "server/internal/sca/check.go" {
		t.Fatalf("context = %v", got)
	}
}
