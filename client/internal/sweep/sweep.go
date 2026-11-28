package sweep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/leotrace-hq/leoprevent-plugin/limits"
	"github.com/leotrace-hq/leoprevent-plugin/pathgate"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

const (
	SkipUnsafePath = "path outside the repository"
	SkipSecret     = "secret file, never sent"
	SkipSymlink    = "symlink, never followed"
	SkipMissing    = "not in this checkout"
	SkipTooLarge   = "larger than a review can carry"
	SkipBinary     = "not text"
	SkipTooMany    = "more findings than one request can judge"
	SkipNoLines    = "the flagged lines are not in this checkout"
	SkipExcluded   = "on a path your organisation excludes from review, never sent"
)

type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type File struct {
	Path    string
	Issues  int
	Content string
	Start   int
	Masked  bool
}

func ReadExcept(patterns []string) func(string, string) (string, string) {
	return func(root string,
		path string) (string, string) {
		if _, ok := pathgate.ExcludedPath(patterns, path); ok {
			return "", SkipExcluded
		}
		return Read(root, path)
	}
}

func Read(root string,
	path string) (string, string) {
	rel := strings.TrimPrefix(filepath.ToSlash(path), "/")
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || !filepath.IsLocal(clean) {
		return "", SkipUnsafePath
	}
	if pathgate.IsSecretPath(rel) {
		return "", SkipSecret
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", SkipMissing
	}
	defer r.Close()
	if linked(r, clean) {
		return "", SkipSymlink
	}
	f, err := r.Open(clean)
	if err != nil {
		return "", SkipMissing
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", SkipMissing
	}
	onDisk, err := r.Lstat(clean)
	if err != nil {
		return "", SkipMissing
	}
	if onDisk.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, onDisk) {
		return "", SkipSymlink
	}
	if !opened.Mode().IsRegular() {
		return "", SkipMissing
	}
	if opened.Size() > limits.MaxChangedFileBytes {
		return "", SkipTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(f, limits.MaxChangedFileBytes+1))
	if err != nil {
		return "", SkipMissing
	}
	if len(b) > limits.MaxChangedFileBytes {
		return "", SkipTooLarge
	}
	if !utf8.Valid(b) || strings.ContainsRune(string(b), '\x00') {
		return "", SkipBinary
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", SkipMissing
	}
	return string(b), ""
}

func linked(r *os.Root,
	clean string) bool {
	cur := ""
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		info, err := r.Lstat(cur)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func Batches(files []File,
	maxBytes int,
	maxFiles int,
	maxIssues int) [][]File {
	var out [][]File
	var cur []File
	bytes, issues := 0, 0
	for _, f := range files {
		if len(cur) > 0 && (bytes+len(f.Content) > maxBytes || len(cur)+1 > maxFiles || issues+f.Issues > maxIssues) {
			out = append(out, cur)
			cur, bytes, issues = nil, 0, 0
		}
		cur = append(cur, f)
		bytes += len(f.Content)
		issues += f.Issues
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

const PageFindings = 25

func Page(files []wire.SweepFile,
	swept []string,
	limit int) (page []wire.SweepFile, rest []wire.SweepFile) {
	done := map[string]bool{}
	for _, p := range swept {
		done[p] = true
	}
	issues := 0
	for _, f := range files {
		switch {
		case done[f.Path]:
		case len(rest) == 0 && (len(page) == 0 || issues+f.Issues <= limit):
			page = append(page, f)
			issues += f.Issues
		default:
			rest = append(rest, f)
		}
	}
	return page, rest
}

type Pending struct {
	ReviewID string         `json:"review_id"`
	Findings []wire.Finding `json:"findings"`
}

type Open struct {
	ReviewID string         `json:"review_id"`
	Commit   string         `json:"commit"`
	Findings []wire.Finding `json:"findings"`
	At       time.Time      `json:"at"`
}

// State is one repository's sweep series for one scanner. Tool names the scanner ("aikido",
// "snyk") and picks the directory it is saved in, so the two sweeps never share a series.
type State struct {
	Tool    string    `json:"-"`
	Repo    string    `json:"repo"`
	Pending []Pending `json:"pending"`
	Open    []Open    `json:"open,omitempty"`
	Swept   []string  `json:"swept,omitempty"`
}

const OpenTTL = 14 * 24 * time.Hour

func (st *State) DropExpired(now time.Time) {
	kept := st.Open[:0]
	for _, o := range st.Open {
		if now.Sub(o.At) < OpenTTL && len(o.Findings) > 0 {
			kept = append(kept, o)
		}
	}
	st.Open = kept
}

var userConfigDir = os.UserConfigDir

func SetUserConfigDirForTest(dir string) func() {
	prev := userConfigDir
	userConfigDir = func() (string, error) { return dir, nil }
	return func() { userConfigDir = prev }
}

func stateDir(tool string) (string, error) {
	if tool == "" || strings.ContainsAny(tool, `/\.`) {
		return "", errors.New("no scanner named for the saved sweep")
	}
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "leoprevent", tool+"-sweep"), nil
}

func stateName(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:8]) + ".json"
}

func Load(tool string,
	repo string) (State, error) {
	st := State{Tool: tool, Repo: repo}
	dir, err := stateDir(tool)
	if err != nil {
		return st, err
	}
	r, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer r.Close()
	b, err := r.ReadFile(stateName(repo))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	st.Tool, st.Repo = tool, repo
	return st, nil
}

func Save(st State) error {
	dir, err := stateDir(st.Tool)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.WriteFile(stateName(st.Repo), b, 0o600)
}

func (st *State) Take(reviewID string) (Pending, bool) {
	for i, p := range st.Pending {
		if p.ReviewID == reviewID {
			st.Pending = append(st.Pending[:i], st.Pending[i+1:]...)
			return p, true
		}
	}
	return Pending{}, false
}

func Changed(root string,
	o Open,
	others []wire.ChangedFile,
	before func(root, rev, path string) (string, bool),
	read func(root, path string) (string, string)) (touched, untouched []wire.Finding, b, a []wire.ChangedFile) {
	flagged := map[string]bool{}
	for _, f := range o.Findings {
		flagged[wire.ParseLocation(f.Location).Path] = true
	}
	elsewhere := OtherChanges(root, others, flagged, read)
	type pair struct {
		ok bool
	}
	files := map[string]pair{}
	for _, f := range o.Findings {
		path := wire.ParseLocation(f.Location).Path
		p, seen := files[path]
		if !seen {
			old, okOld := before(root, o.Commit, path)
			now, skip := read(root, path)
			changed := okOld && skip == "" && old != now
			p = pair{ok: okOld && skip == "" && (changed || len(elsewhere) > 0)}
			files[path] = p
			if p.ok {
				b = append(b, wire.ChangedFile{Path: path, FullContent: old})
				a = append(a, wire.ChangedFile{Path: path, FullContent: now})
			}
		}
		if p.ok {
			touched = append(touched, f)
		} else {
			untouched = append(untouched, f)
		}
	}
	if len(touched) > 0 {
		a = append(a, elsewhere...)
	}
	return touched, untouched, b, a
}

func OtherChanges(root string,
	others []wire.ChangedFile,
	flagged map[string]bool,
	read func(root, path string) (string, string)) []wire.ChangedFile {
	var out []wire.ChangedFile
	size := 0
	for _, c := range others {
		if flagged[c.Path] {
			continue
		}
		_, skip := read(root, c.Path)
		if !(skip == "" && !c.Deleted) && !(c.Deleted && skip == SkipMissing) {
			continue
		}
		n := len(c.AddedText) + len(c.RemovedText)
		if n == 0 || size+n > limits.MaxReviewBytes {
			continue
		}
		size += n
		out = append(out, c)
		if len(out) == wire.MaxSweepContextFiles {
			break
		}
	}
	return out
}

func GuardContext(root string,
	guards []wire.SweepGuard,
	swept map[string]bool,
	tracked []string,
	read func(string, string) (string, string),
	maxBytes int) []wire.ContextFile {
	var out []wire.ContextFile
	seen := map[string]bool{}
	size := 0
	for _, want := range guards {
		path := trackedPath(want.Path, tracked)
		if path == "" {
			path = definingFile(root, want, tracked, read)
		}
		if path == "" || swept[path] || seen[path] {
			continue
		}
		content, skip := read(root, path)
		if skip != "" || strings.TrimSpace(content) == "" || size+len(content) > maxBytes {
			continue
		}
		seen[path] = true
		size += len(content)
		out = append(out, wire.ContextFile{Path: path, Content: content})
		if len(out) == wire.MaxSweepContextFiles {
			break
		}
	}
	return out
}

var sourceExts = map[string]bool{".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".py": true, ".rb": true, ".java": true, ".kt": true, ".rs": true, ".php": true, ".cs": true, ".swift": true}

func definingFile(root string,
	guard wire.SweepGuard,
	tracked []string,
	read func(string, string) (string, string)) string {
	if wire.GuardName(guard.Symbol) == "" {
		return ""
	}
	hints := guardHints(guard.Path)
	best, bestScore, tied := "", -1, false
	for _, t := range tracked {
		if !sourceExts[strings.ToLower(filepath.Ext(t))] || isTestPath(t) {
			continue
		}
		content, skip := read(root, t)
		if skip != "" || !wire.DefinesSymbol(content, guard.Symbol) {
			continue
		}
		parts := map[string]bool{}
		for _, part := range pathParts(t) {
			parts[part] = true
		}
		score := 0
		for _, h := range hints {
			if parts[h] {
				score++
			}
		}
		switch {
		case score > bestScore:
			best, bestScore, tied = t, score, false
		case score == bestScore:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

func pathParts(path string) []string {
	return strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' || r == '@' || r == '.' || r == '\\' })
}

func guardHints(path string) []string {
	var out []string
	for _, part := range pathParts(path) {
		if len(part) >= 3 && part != "src" && part != "lib" && part != "internal" && part != "server" {
			out = append(out, part)
		}
	}
	return out
}

func isTestPath(p string) bool {
	l := strings.ToLower(p)
	return strings.HasSuffix(l, "_test.go") || strings.Contains(l, ".test.") || strings.Contains(l, ".spec.") ||
		strings.Contains(l, "/testdata/") || strings.Contains(l, "/__tests__/")
}

func NeedsOf(resp wire.SweepResponse) []wire.SweepGuard {
	if len(resp.NeedsGuards) > 0 {
		return resp.NeedsGuards
	}
	out := make([]wire.SweepGuard, 0, len(resp.NeedsContext))
	for _, p := range resp.NeedsContext {
		out = append(out, wire.SweepGuard{Path: p})
	}
	return out
}

func trackedPath(want string,
	tracked []string) string {
	want = strings.TrimPrefix(strings.TrimSpace(filepath.ToSlash(want)), "./")
	if want == "" {
		return ""
	}
	hit, n := "", 0
	for _, t := range tracked {
		if t == want {
			return t
		}
		if strings.HasSuffix(t, "/"+want) {
			hit = t
			n++
		}
	}
	if n == 1 {
		return hit
	}
	return ""
}

const MaxContextRounds = 4

func WithGuards(first wire.SweepResponse,
	req wire.SweepRequest,
	send func(wire.SweepRequest) (wire.SweepResponse, error),
	fetch func(need []wire.SweepGuard, have []wire.ContextFile) []wire.ContextFile) (wire.SweepResponse, error) {
	resp := first
	var context []wire.ContextFile
	for round := 1; len(resp.NeedsContext) > 0 && round <= MaxContextRounds; round++ {
		added := fetch(NeedsOf(resp), context)
		if room := wire.MaxSweepContextFiles - len(context); len(added) > room {
			added = added[:max(room, 0)]
		}
		context = append(context, added...)
		next := req
		next.Context = context
		next.ContextSent = round == MaxContextRounds || len(added) == 0 || len(context) >= wire.MaxSweepContextFiles
		again, err := send(next)
		if err != nil {
			return resp, err
		}
		resp = again
		if next.ContextSent {
			break
		}
	}
	return resp, nil
}

func ContextBytes(files []wire.ContextFile) int {
	n := 0
	for _, f := range files {
		n += len(f.Content)
	}
	return n
}
