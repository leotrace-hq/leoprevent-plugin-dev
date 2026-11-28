package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

func parse(
	t *testing.T,
	a *Adapter,
	stdin string,
) agent.Event {
	t.Helper()
	ev, err := a.ParseEvent([]byte(stdin))
	if err != nil {
		t.Fatalf("parse %s: %v", stdin, err)
	}
	return ev
}

func TestOnlyTheFirstInvocationOfATurnTakesTheBaseline(
	t *testing.T,
) {
	// Arrange
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	a := New()
	first := `{"hookEventName":"PreInvocation","conversationId":"c1","cwd":"/repo","lastUserInput":"add login","invocationNum":0}`
	later := `{"hookEventName":"PreInvocation","conversationId":"c1","cwd":"/repo","lastUserInput":"add login","invocationNum":4}`

	// Act
	start := parse(t, a, first)
	mid := parse(t, a, later)

	// Assert
	if !start.IsUserPromptSubmit() || start.SessionID != "c1" || start.Cwd != "/repo" {
		t.Fatalf("turn start=%+v", start)
	}
	if mid.IsUserPromptSubmit() || !mid.IsAgentResponse() {
		t.Fatalf("a mid-turn invocation re-baselined: %+v", mid)
	}
}

func TestTheReviewWakeDoesNotReBaselineButTheNextPromptDoes(
	t *testing.T,
) {
	// Arrange
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	a := New()
	parse(t, a, `{"hookEventName":"PreInvocation","conversationId":"c2","lastUserInput":"add login"}`)
	parse(t, a, `{"hookEventName":"Stop","conversationId":"c2","terminationReason":"NO_TOOL_CALL","fullyIdle":true}`)
	if _, err := a.DeliverReview("fix it", "banner", 1, nil, ""); err != nil {
		t.Fatal(err)
	}

	// Act
	woken := parse(t, a, `{"hookEventName":"PreInvocation","conversationId":"c2","lastUserInput":"add login"}`)
	stop := parse(t, a, `{"hookEventName":"Stop","conversationId":"c2","terminationReason":"NO_TOOL_CALL","fullyIdle":true}`)
	next := parse(t, a, `{"hookEventName":"PreInvocation","conversationId":"c2","lastUserInput":"add login"}`)

	// Assert
	if woken.IsUserPromptSubmit() {
		t.Fatal("the review's own continuation re-baselined the turn")
	}
	if !stop.StopHookActive {
		t.Fatal("the Stop after our own wake was reviewed as a new turn")
	}
	if !next.IsUserPromptSubmit() {
		t.Fatal("a new prompt after a finished turn kept the old baseline")
	}
}

func TestAChangedPromptStartsATurnEvenWithoutAStop(
	t *testing.T,
) {
	// Arrange
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	a := New()
	parse(t, a, `{"hookEventName":"PreInvocation","conversationId":"c3","lastUserInput":"one"}`)

	// Act
	ev := parse(t, a, `{"hookEventName":"PreInvocation","conversationId":"c3","lastUserInput":"two"}`)

	// Assert
	if !ev.IsUserPromptSubmit() {
		t.Fatalf("event=%+v", ev)
	}
}

func TestPreToolUseReadsAntigravityToolArguments(
	t *testing.T,
) {
	// Arrange
	a := New()

	// Act
	write := parse(t, a, `{"hookEventName":"PreToolUse","conversationId":"c4","toolCall":{"name":"write_to_file","args":{"TargetFile":"/repo/app.py"}}}`)
	shell := parse(t, a, `{"hookEventName":"PreToolUse","conversationId":"c4","toolCall":{"name":"run_command","args":{"CommandLine":"sed -i s/a/b/ /repo/app.py"}}}`)

	// Assert
	if !write.IsPreToolUse() || len(write.EditPaths) != 1 || write.EditPaths[0] != "/repo/app.py" {
		t.Fatalf("write=%+v", write)
	}
	if !shell.IsPreToolUse() || len(shell.EditPaths) == 0 {
		t.Fatalf("shell=%+v", shell)
	}
}

func TestUnknownOrUnfinishedEventsNeverReachStop(
	t *testing.T,
) {
	for _, stdin := range []string{
		`{"hookEventName":"PostToolUse","conversationId":"c5"}`,
		`{"hookEventName":"SessionStart","conversationId":"c5"}`,
		`{"hookEventName":"PostInvocation","conversationId":"c5"}`,
		`{"hookEventName":"Stop","conversationId":"c5","terminationReason":"USER_CANCELED"}`,
		`{"hookEventName":"Stop","conversationId":"c5","terminationReason":"ERROR"}`,
		`{"hookEventName":"Stop","conversationId":"c5","terminationReason":"MAX_FORCED_INVOCATIONS"}`,
	} {
		t.Run(stdin, func(t *testing.T) {
			// Act
			_, err := New().ParseEvent([]byte(stdin))

			// Assert
			if err == nil {
				t.Fatal("reached the review path")
			}
		})
	}
}

func TestReviewUsesTheContinueDecisionStopAccepts(
	t *testing.T,
) {
	// Arrange
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	a := New()
	parse(t, a, `{"hookEventName":"Stop","conversationId":"c6","terminationReason":"NO_TOOL_CALL"}`)

	// Act
	out, err := a.DeliverReview("repair", "review", 1, nil, "")
	relayed, _, relayErr := a.RelayForDeveloper(out, "notice", "notice", "")
	woken, wakeErr := a.WakeForDependencies(out, "check deps")

	// Assert
	for _, got := range []struct {
		out []byte
		err error
	}{{out, err}, {relayed, relayErr}, {woken, wakeErr}} {
		var hook map[string]any
		if got.err != nil || json.Unmarshal(got.out, &hook) != nil || hook["decision"] != "continue" || hook["reason"] == "" {
			t.Fatalf("output=%s err=%v", got.out, got.err)
		}
		if len(hook) != 2 {
			t.Fatalf("Antigravity's Stop output accepts only decision and reason: %s", got.out)
		}
	}
}

func TestNoticesNeverBlock(
	t *testing.T,
) {
	// Act
	out, err := New().DeliverNotice("unreviewed")

	// Assert
	var hook map[string]any
	if err != nil || json.Unmarshal(out, &hook) != nil || hook["decision"] != nil {
		t.Fatalf("output=%s err=%v", out, err)
	}
}

func TestPromptNoticeInjectsASystemMessage(
	t *testing.T,
) {
	// Act
	out, err := New().DeliverPromptNotice("shown", "for the model")

	// Assert
	var hook struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string            `json:"systemMessage"`
				Metadata      map[string]string `json:"metadata"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err != nil || json.Unmarshal(out, &hook) != nil || len(hook.InjectSteps) != 1 {
		t.Fatalf("output=%s err=%v", out, err)
	}
	step := hook.InjectSteps[0].SystemMessage
	if step.SystemMessage != "for the model" || step.Metadata["user_markdown"] != "shown" {
		t.Fatalf("step=%+v", step)
	}
}

func TestEnvironmentFollowsTheTranscriptDirectory(
	t *testing.T,
) {
	for _, tc := range []struct{ path, want string }{
		{"/Users/x/.gemini/antigravity/brain/c/.system_generated/logs/transcript.jsonl", wire.EnvAntigravityApp},
		{"/Users/x/.gemini/antigravity-cli/brain/c/.system_generated/logs/transcript.jsonl", wire.EnvAntigravityCLI},
		{"/Users/x/.gemini/antigravity-ide/brain/c/.system_generated/logs/transcript.jsonl", wire.EnvAntigravityIDE},
		{"", wire.EnvUnknown},
	} {
		t.Run(tc.want, func(t *testing.T) {
			// Arrange
			a := New()
			data, _ := json.Marshal(map[string]any{"hookEventName": "PreToolUse", "transcriptPath": tc.path})
			parse(t, a, string(data))

			// Act
			got := a.Environment(agent.Event{}).Name

			// Assert
			if got != tc.want {
				t.Fatalf("environment=%q", got)
			}
		})
	}
}

func TestOnlyTheStopRightAfterAWakeIsGuarded(
	t *testing.T,
) {
	// Arrange
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	a := New()
	stopJSON := `{"hookEventName":"Stop","conversationId":"c7","terminationReason":"NO_TOOL_CALL"}`
	parse(t, a, `{"hookEventName":"PreInvocation","conversationId":"c7","lastUserInput":"one"}`)
	first := parse(t, a, stopJSON)
	if _, err := a.DeliverReview("fix it", "banner", 1, nil, ""); err != nil {
		t.Fatal(err)
	}
	guarded := parse(t, a, stopJSON)
	parse(t, a, `{"hookEventName":"PreInvocation","conversationId":"c7","lastUserInput":"two"}`)

	// Act
	next := parse(t, a, stopJSON)

	// Assert
	if first.StopHookActive || !guarded.StopHookActive || next.StopHookActive {
		t.Fatalf("first=%v guarded=%v next=%v", first.StopHookActive, guarded.StopHookActive, next.StopHookActive)
	}
}

func TestReviewAsksForTheNoticeInTheVisibleReply(
	t *testing.T,
) {
	// Arrange
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	a := New()
	parse(t, a, `{"hookEventName":"Stop","conversationId":"c8","terminationReason":"NO_TOOL_CALL"}`)
	findings := []wire.Finding{{Rule: "sql-injection", Name: "SQL Injection", Location: "app.py:3"}}

	// Act
	out, err := a.DeliverReview("repair", "banner", 1, findings, "")

	// Assert
	var hook map[string]string
	if err != nil || json.Unmarshal(out, &hook) != nil {
		t.Fatalf("output=%s err=%v", out, err)
	}
	if !strings.Contains(hook["reason"], "LeoPrevent") || !strings.Contains(hook["reason"], "final, user-visible response") {
		t.Fatalf("reason=%s", hook["reason"])
	}
}
