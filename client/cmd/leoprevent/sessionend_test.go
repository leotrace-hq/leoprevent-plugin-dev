package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/config"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
)

// sessionEndFixture is a server that records every path it is asked for, a Claude config with
// one enabled plugin (so the snapshot is not empty) and a session whose last turn WROTE CODE: if
// the end of the session ever fell through to the Stop path, that turn would be reviewed and the
// server would see /review.
type sessionEndFixture struct {
	mu       sync.Mutex
	requests []string
	cwd      string
	tp       string
	// accountOff makes the server answer the policy check with the account's agent inventory
	// switch off, as it does for every account staff have not switched on.
	accountOff bool
}

func (f *sessionEndFixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.requests {
		if p == path {
			n++
		}
	}
	return n
}

func newSessionEndFixture(t *testing.T) *sessionEndFixture {
	t.Helper()
	f := &sessionEndFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.Path)
		off := f.accountOff
		f.mu.Unlock()
		if r.URL.Path == "/policy/check" {
			w.Header().Set("Content-Type", "application/json")
			if off {
				_, _ = w.Write([]byte(`{"excluded":false}`))
			} else {
				_, _ = w.Write([]byte(`{"excluded":false,"agent_inventory":true}`))
			}
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvServerURL, srv.URL)
	t.Setenv(config.EnvTier, config.TierCloud)
	t.Setenv(config.EnvLicenseKey, "test-license")
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	t.Cleanup(config.SetUserConfigDirForTest(t.TempDir()))

	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for rel, content := range map[string]string{
		"settings.json":                  `{"enabledPlugins": {"leoprevent@leotrace": true}}`,
		"plugins/installed_plugins.json": `{"plugins": {"leoprevent@leotrace": [{"scope": "user", "version": "0.2.48"}]}}`,
		"projects/p/s.jsonl": `{"type":"user","version":"2.1.317","permissionMode":"acceptEdits","message":{"role":"user","content":"add a refresh route"}}` + "\n" +
			`{"type":"assistant","version":"2.1.317","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/p/app.py","content":"url = request.args['url']\nreturn requests.get(url).text"}}]}}` + "\n",
	} {
		p := filepath.Join(cfg, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.cwd = t.TempDir()
	f.tp = filepath.Join(cfg, "projects", "p", "s.jsonl")
	return f
}

func (f *sessionEndFixture) end(t *testing.T, session string) (int, string) {
	t.Helper()
	payload := fmt.Sprintf(`{"hook_event_name":"SessionEnd","reason":"other","session_id":%q,"cwd":%q,"transcript_path":%q}`, session, f.cwd, f.tp)
	code, stdout, _ := runWith(t, []string{"--agent=claude"}, payload)
	return code, stdout
}

// ⚠️ THE END OF A SESSION IS NOT A STOP. The Claude manifest registers SessionEnd, and anything
// main does not route falls through to the review. It must send the session's inventory snapshot
// and nothing else: no review, no telemetry, nothing on stdout, exit 0.
func TestSessionEndSendsOnlyTheInventory(t *testing.T) {
	f := newSessionEndFixture(t)
	code, stdout := f.end(t, "end-1")
	if code != 0 || stdout != "" {
		t.Errorf("code=%d stdout=%q, want 0 and nothing printed", code, stdout)
	}
	if f.count("/review") != 0 || f.count("/telemetry") != 0 || f.count("/outcome") != 0 {
		t.Errorf("the end of a session ran the Stop path: requests=%q", f.requests)
	}
	if f.count("/inventory") != 1 {
		t.Errorf("requests=%q, want exactly one /inventory", f.requests)
	}

	// The machine has now reported today, so the end of another short session sends none.
	if code, stdout := f.end(t, "end-2"); code != 0 || stdout != "" {
		t.Errorf("code=%d stdout=%q, want 0 and nothing printed", code, stdout)
	}
	if f.count("/inventory") != 1 || f.count("/review") != 0 {
		t.Errorf("requests=%q, want no second /inventory and no /review", f.requests)
	}
}

// With the inventory switched off, or with no license key, the end of a session sends nothing.
func TestSessionEndSendsNothingWhenOffOrUnlicensed(t *testing.T) {
	t.Run("switched off", func(t *testing.T) {
		f := newSessionEndFixture(t)
		t.Setenv(config.EnvAgentInventory, "0")
		if code, stdout := f.end(t, "end-off"); code != 0 || stdout != "" {
			t.Errorf("code=%d stdout=%q", code, stdout)
		}
		if len(f.requests) != 0 {
			t.Errorf("requests=%q, want none", f.requests)
		}
	})
	// The account's switch, which staff turn on per customer: without it nothing is read off the
	// computer and nothing is sent to /inventory. The policy check that asks for it carries the
	// repository only, as on every turn.
	t.Run("account not switched on", func(t *testing.T) {
		f := newSessionEndFixture(t)
		f.accountOff = true
		if code, stdout := f.end(t, "end-account-off"); code != 0 || stdout != "" {
			t.Errorf("code=%d stdout=%q", code, stdout)
		}
		if f.count("/inventory") != 0 || f.count("/review") != 0 || f.count("/telemetry") != 0 {
			t.Errorf("requests=%q, want no /inventory", f.requests)
		}
	})
	t.Run("no license key", func(t *testing.T) {
		f := newSessionEndFixture(t)
		t.Setenv(config.EnvLicenseKey, "")
		if code, stdout := f.end(t, "end-nokey"); code != 0 || stdout != "" {
			t.Errorf("code=%d stdout=%q", code, stdout)
		}
		if len(f.requests) != 0 {
			t.Errorf("requests=%q, want none", f.requests)
		}
	})
}

// Another agent's binary handed a SessionEnd does nothing: only Claude's adapter reports an
// inventory, and the event must still never reach the review.
func TestSessionEndOnAnotherAgentIsANoOp(t *testing.T) {
	f := newSessionEndFixture(t)
	payload := fmt.Sprintf(`{"hook_event_name":"SessionEnd","session_id":"end-codex","cwd":%q}`, f.cwd)
	code, stdout, _ := runWith(t, []string{"--agent=codex"}, payload)
	if code != 0 || stdout != "" || len(f.requests) != 0 {
		t.Errorf("code=%d stdout=%q requests=%q, want 0, nothing printed and no request", code, stdout, f.requests)
	}
}
