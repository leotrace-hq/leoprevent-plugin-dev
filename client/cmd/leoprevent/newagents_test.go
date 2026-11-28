package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/config"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/outcome"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

func TestAgentCloudReviewAndRepairOutcome(
	t *testing.T,
) {
	for _, name := range []string{"cursor", "devin", "copilot", "gemini", "hermes", "antigravity"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var reviewed, rewake wire.ReviewRequest
			var repaired wire.OutcomeRequest
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer test-license" {
					t.Error("review credential missing")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/review":
					var req wire.ReviewRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.Meta.RewakeOf != "" {
						rewake = req
					} else {
						reviewed = req
					}
					_, _ = w.Write([]byte(`{"verdict":"triggered","review_id":"test-review","findings":[{"rule":"sql-injection","name":"SQL Injection","location":"app.py:3","issue":"SQL input is concatenated","fix":"Parameterize the query"}]}`))
				case "/outcome":
					if err := json.NewDecoder(r.Body).Decode(&repaired); err != nil {
						t.Error(err)
					}
					_, _ = w.Write([]byte(`{}`))
				default:
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer srv.Close()
			t.Setenv(config.EnvServerURL, srv.URL)
			t.Setenv(config.EnvTier, config.TierCloud)
			t.Setenv(config.EnvLicenseKey, "test-license")
			t.Cleanup(config.SetUserConfigDirForTest(t.TempDir()))
			// Each subtest gets its own plugin state, so the per-license policy cache (and every
			// other cache) never carries over from another subtest or an earlier run.
			t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
			dir := t.TempDir()
			session := name + filepath.Base(dir)
			t.Cleanup(func() { outcome.Clear(session); outcome.ClearLedger(session) })
			if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git: %v %s", err, out)
			}
			var geminiPrompt string
			payload := func(event, prompt string, active bool) string {
				p := map[string]any{"hook_event_name": event, "session_id": session, "cwd": dir, "prompt": prompt, "stop_hook_active": active}
				if name == "cursor" {
					p["conversation_id"], p["workspace_roots"], p["status"] = session, []string{dir}, "completed"
					if event == "UserPromptSubmit" {
						p["hook_event_name"] = "beforeSubmitPrompt"
					} else {
						p["hook_event_name"] = "stop"
					}
				}
				if name == "gemini" {
					p["hook_event_name"] = "AfterAgent"
					p["stop_hook_active"] = false
					// Like Gemini, AfterAgent repeats the turn's prompt: the developer's on the
					// first stop, the resubmitted review reason on the fix turn.
					if event == "UserPromptSubmit" {
						p["hook_event_name"] = "BeforeAgent"
						geminiPrompt = prompt
					} else if prompt == "" {
						p["prompt"] = geminiPrompt
					}
				}
				if name == "hermes" {
					attempt := 0
					if active {
						attempt = 1
					}
					p = map[string]any{"session_id": session, "cwd": dir, "extra": map[string]any{"user_message": prompt, "attempt": attempt, "final_response": "done"}}
					if event == "UserPromptSubmit" {
						p["hook_event_name"] = "pre_llm_call"
					} else {
						p["hook_event_name"] = "pre_verify"
					}
				}
				if name == "antigravity" {
					p = map[string]any{"conversationId": session, "cwd": dir, "transcriptPath": filepath.Join(dir, ".gemini", "antigravity-cli", "transcript.jsonl"), "lastUserInput": "add query", "continuationActive": active, "terminationReason": "NO_TOOL_CALL", "fullyIdle": true}
					if event == "UserPromptSubmit" {
						p["hookEventName"] = "PreInvocation"
					} else {
						p["hookEventName"] = "Stop"
					}
				}
				data, _ := json.Marshal(p)
				return string(data)
			}
			args := []string{"--agent=" + name}
			_, _, _ = runWith(t, args, payload("UserPromptSubmit", "add query", false))
			file := filepath.Join(dir, "app.py")
			if err := os.WriteFile(file, []byte("import sqlite3\nconn = sqlite3.connect('app.db')\nconn.execute('SELECT * FROM users WHERE name = ' + name)\n"), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runWith(t, args, payload("Stop", "", false))
			var feedback map[string]any
			if code != 0 || json.Unmarshal([]byte(out), &feedback) != nil {
				t.Fatalf("review code=%d out=%s stderr=%s", code, out, stderr)
			}
			if name == "cursor" {
				prompt, ok := feedback["followup_message"].(string)
				if !ok || prompt == "" {
					t.Fatalf("missing followup: %s", out)
				}
				_, _, _ = runWith(t, args, payload("UserPromptSubmit", prompt, false))
			} else if name == "antigravity" {
				if feedback["decision"] != "continue" {
					t.Fatalf("missing continue: %s", out)
				}
			} else if feedback["decision"] != "block" {
				t.Fatalf("missing block: %s", out)
			}
			if name == "gemini" {
				reason, ok := feedback["reason"].(string)
				if !ok || reason == "" {
					t.Fatalf("missing Gemini retry prompt: %s", out)
				}
				_, _, _ = runWith(t, args, payload("UserPromptSubmit", reason, false))
			}
			if name == "copilot" {
				nested, ok := feedback["hookSpecificOutput"].(map[string]any)
				if !ok || nested["decision"] != "block" {
					t.Fatalf("VS Code feedback missing: %s", out)
				}
			}
			if err := os.WriteFile(file, []byte("import sqlite3\nconn = sqlite3.connect('app.db')\nconn.execute('SELECT * FROM users WHERE name = ?', (name,))\n"), 0600); err != nil {
				t.Fatal(err)
			}
			code, _, stderr = runWith(t, args, payload("Stop", "", true))
			if code != 0 {
				t.Fatalf("guard code=%d stderr=%s", code, stderr)
			}
			mu.Lock()
			defer mu.Unlock()
			// The organisation's policy check runs once, before anything else is sent (a fresh
			// cache), and its answer is reused for the fix turn. The rest is the review loop.
			if len(paths) == 0 || paths[0] != "/policy/check" || slices.Contains(paths[1:], "/policy/check") {
				t.Fatalf("policy check first, then cached: requests=%v", paths)
			}
			rest := paths[1:]
			if len(rest) != 3 || rest[0] != "/review" || !slices.Contains(rest[1:], "/outcome") || !slices.Contains(rest[1:], "/review") {
				t.Fatalf("requests=%v", paths)
			}
			if rewake.Meta.RewakeOf != "test-review" || len(rewake.Changes) != 1 || !strings.Contains(rewake.Changes[0].AddedText, "(name,)") {
				t.Fatalf("fix-turn review=%+v", rewake)
			}
			if reviewed.Meta.Agent != name || len(reviewed.Changes) != 1 {
				t.Fatalf("review=%+v", reviewed)
			}
			if repaired.ReviewID != "test-review" || len(repaired.After) != 1 || !strings.Contains(repaired.After[0].FullContent, "(name,)") {
				t.Fatalf("outcome=%+v", repaired)
			}
			if name == "gemini" {
				// The resubmitted reason must not reset the baseline: measured from before the
				// turn, the outcome's app.py is still a whole new file, not a one-line edit of
				// the flawed version (which is what a reset baseline reports).
				if !strings.Contains(repaired.After[0].AddedText, "import sqlite3") {
					t.Fatalf("outcome measured against a new baseline: %+v", repaired.After[0])
				}
				// Nor may the review reason reach the outcome as the developer's prompt.
				if repaired.Prompt != "add query" {
					t.Fatalf("outcome prompt=%q, want the developer's", repaired.Prompt)
				}
			}
		})
	}
}
