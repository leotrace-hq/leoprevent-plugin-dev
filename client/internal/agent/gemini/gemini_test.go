package gemini

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
)

func TestBeforeAgentPreservesBaselineOnlyForExactWake(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	runWake := func(reason string) {
		a := New()
		if _, err := a.ParseEvent([]byte(`{"hook_event_name":"AfterAgent","session_id":"session"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.DeliverReview(reason, "Review completed", 1, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	before := func(prompt string) bool {
		input, err := json.Marshal(map[string]string{
			"hook_event_name": "BeforeAgent",
			"session_id":      "session",
			"prompt":          prompt,
		})
		if err != nil {
			t.Fatal(err)
		}
		event, err := New().ParseEvent(input)
		if err != nil || !event.IsUserPromptSubmit() {
			t.Fatalf("unexpected event %+v: %v", event, err)
		}
		return event.ReviewContinuation
	}
	runWake("Apply the review feedback.")
	if before("A fresh request") {
		t.Fatal("an unrelated prompt reused the old baseline")
	}
	if before("Apply the review feedback.") {
		t.Fatal("an interrupted wake survived a fresh prompt")
	}
	runWake("Apply the review feedback.")
	if !before("Apply the review feedback.") {
		t.Fatal("the wake failed to preserve the baseline")
	}
	stop := func() bool {
		event, err := New().ParseEvent([]byte(`{"hook_event_name":"AfterAgent","session_id":"session","stop_hook_active":false}`))
		if err != nil {
			t.Fatal(err)
		}
		return event.StopHookActive
	}
	if !stop() || stop() {
		t.Fatal("the matching retry must be recognized at exactly its first Stop")
	}
	runWake("Apply the review feedback.")
	if !before("Apply the review feedback.") || before("A fresh request") || stop() {
		t.Fatal("a fresh request must clear an interrupted retry")
	}
}

func TestGeminiToolAndStopEvents(t *testing.T) {
	a := New()
	dir := filepath.Join("workspace", "subdir")
	tool, err := a.ParseEvent([]byte(`{"hook_event_name":"BeforeTool","tool_name":"run_shell_command","tool_input":{"dir_path":"workspace/subdir","command":"touch example.py"}}`))
	if err != nil || !tool.IsPreToolUse() || !slices.Contains(tool.TouchedPaths, filepath.Join(dir, "example.py")) {
		t.Fatalf("unexpected tool event %+v: %v", tool, err)
	}
	first, err := a.ParseEvent([]byte(`{"hook_event_name":"AfterAgent","prompt_response":"original reply","stop_hook_active":false}`))
	if err != nil || first.Name != "Stop" || first.StopHookActive || first.LastAssistantMessage != "original reply" {
		t.Fatalf("unexpected first Stop %+v: %v", first, err)
	}
	retry, err := a.ParseEvent([]byte(`{"hook_event_name":"AfterAgent","prompt_response":"original reply\nnew reply","stop_hook_active":true}`))
	if err != nil || !retry.StopHookActive || retry.LastAssistantMessage != "" {
		t.Fatalf("unexpected retry Stop %+v: %v", retry, err)
	}
	if _, err := a.ParseEvent([]byte(`{"hook_event_name":"AfterModel"}`)); err == nil {
		t.Fatal("an unregistered event reached the review path")
	}
}

func TestReviewIsDeliveredWhenTheRetryMarkerCannotBeSaved(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	a := New() // no session_id: the marker cannot be written
	out, err := a.DeliverReview("Fix the injection.", "Review completed", 1, nil, "")
	var block map[string]any
	if err != nil || json.Unmarshal(out, &block) != nil || block["decision"] != "block" {
		t.Fatalf("the review must still block: out=%s err=%v", out, err)
	}
}

func TestRetryReportsTheDevelopersPromptNotTheReviewReason(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	parse := func(a *Adapter, in map[string]any) {
		data, _ := json.Marshal(in)
		if _, err := a.ParseEvent(data); err != nil {
			t.Fatal(err)
		}
	}
	first := New()
	parse(first, map[string]any{"hook_event_name": "AfterAgent", "session_id": "s", "prompt": "add a search endpoint"})
	if _, err := first.DeliverReview("Fix the injection.", "Review completed", 1, nil, ""); err != nil {
		t.Fatal(err)
	}
	parse(New(), map[string]any{"hook_event_name": "BeforeAgent", "session_id": "s", "prompt": "Fix the injection."})
	retry := New()
	parse(retry, map[string]any{"hook_event_name": "AfterAgent", "session_id": "s", "prompt": "Fix the injection.", "stop_hook_active": false})
	meta, _ := retry.TurnMeta(agent.Event{})
	if !retry.reviewRetry || meta.Prompt != "add a search endpoint" {
		t.Fatalf("retry=%v prompt=%q", retry.reviewRetry, meta.Prompt)
	}
	// Gemini's own flag without a matched wake is still a retry, and never reports the reason.
	flagged := New()
	parse(flagged, map[string]any{"hook_event_name": "AfterAgent", "session_id": "s", "prompt": "Fix the injection.", "stop_hook_active": true})
	meta, _ = flagged.TurnMeta(agent.Event{})
	if !flagged.reviewRetry || meta.Prompt != "" {
		t.Fatalf("retry=%v prompt=%q", flagged.reviewRetry, meta.Prompt)
	}
}

func TestShellCommandInADirectoryDiscoversThatDirectory(t *testing.T) {
	ev, err := New().ParseEvent([]byte(`{"hook_event_name":"BeforeTool","tool_name":"run_shell_command","cwd":"/workspace","tool_input":{"dir_path":"repo","command":"printf changed > app.py"}}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ev.EditPaths {
		found = found || filepath.Dir(p) == "repo"
	}
	if !found {
		t.Fatalf("dir_path is not an edit candidate: %v", ev.EditPaths)
	}
}
