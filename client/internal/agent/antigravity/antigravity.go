package antigravity

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/review"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/transcript"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

type Adapter struct {
	conversationID string
	model          string
	prompt         string
	surface        string
}

func New() *Adapter           { return &Adapter{} }
func (*Adapter) Name() string { return "antigravity" }

func (a *Adapter) Environment(
	agent.Event,
) agent.Environment {
	switch a.surface {
	case "antigravity":
		return agent.Environment{Name: wire.EnvAntigravityApp, Raw: a.surface}
	case "antigravity-cli":
		return agent.Environment{Name: wire.EnvAntigravityCLI, Raw: a.surface}
	case "antigravity-ide":
		return agent.Environment{Name: wire.EnvAntigravityIDE, Raw: a.surface}
	default:
		return agent.Environment{Name: wire.EnvUnknown, Raw: a.surface}
	}
}

type payload struct {
	Name                    string   `json:"hookEventName"`
	ConversationID          string   `json:"conversationId"`
	WorkspacePaths          []string `json:"workspacePaths"`
	TranscriptPath          string   `json:"transcriptPath"`
	ModelName               string   `json:"modelName"`
	LastUserInput           string   `json:"lastUserInput"`
	Cwd                     string   `json:"cwd"`
	TerminationReason       string   `json:"terminationReason"`
	ContinuationActive      bool     `json:"continuationActive"`
	ContinuationActiveSnake bool     `json:"continuation_active"`
	ToolCall                struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"toolCall"`
}

func (a *Adapter) ParseEvent(
	stdin []byte,
) (agent.Event, error) {
	var p payload
	if err := json.Unmarshal(stdin, &p); err != nil {
		return agent.Event{}, err
	}
	a.conversationID, a.model, a.prompt = p.ConversationID, p.ModelName, p.LastUserInput
	a.surface = surface(p.TranscriptPath)
	cwd := p.Cwd
	if cwd == "" && len(p.WorkspacePaths) > 0 {
		cwd = p.WorkspacePaths[0]
	}
	ev := agent.Event{SessionID: p.ConversationID, Cwd: cwd}
	switch p.Name {
	case "PreInvocation":
		if !a.beginsTurn(p.LastUserInput) {
			ev.Name = agent.EventAgentResponse
			return ev, nil
		}
		ev.Name = agent.EventUserPromptSubmit
	case "PreToolUse":
		ev.Name = agent.EventPreToolUse
		in := toolInput(p.ToolCall.Args)
		ev.EditPaths = agent.EditPathsFromToolInput(in)
		ev.TouchedPaths = agent.TouchedPathsFromToolInput(in)
	case "Stop":
		if skippedTermination(p.TerminationReason) {
			return agent.Event{}, fmt.Errorf("Antigravity stop reason %q is not a finished turn", p.TerminationReason)
		}
		ev.Name = "Stop"
		woken := a.endTurn()
		ev.StopHookActive = woken || p.ContinuationActive || p.ContinuationActiveSnake
	default:
		return agent.Event{}, fmt.Errorf("unsupported Antigravity event %q", p.Name)
	}
	return ev, nil
}

func (*Adapter) ChangedFiles(
	agent.Event,
) ([]transcript.Change, error) {
	return nil, nil
}

func (a *Adapter) TurnMeta(
	agent.Event,
) (agent.TurnMeta, error) {
	return agent.TurnMeta{AgentModel: a.model, Prompt: a.prompt}, nil
}

func (*Adapter) AgentReply(
	agent.Event,
) (string, error) {
	return "", nil
}

func (a *Adapter) DeliverReview(
	prompt, banner string,
	fileCount int,
	findings []wire.Finding,
	summary string,
) ([]byte, error) {
	notice := review.ReviewContextMessageWith(summary, fileCount, findings, review.ForceFixedCount(findings))
	out, err := review.RewakeJSON(prompt+"\n\n"+notice+"\n\n"+
		"Antigravity collapses this message out of the developer's view. Begin your final, user-visible response after completing the requested repairs with the LeoPrevent markdown notice above, exactly as written. Printing it only before a tool call or in your thinking does not show it to the developer.", banner)
	if err != nil {
		return nil, err
	}
	return a.continued(out)
}

func (a *Adapter) WakeForDependencies(
	out []byte,
	prompt string,
) ([]byte, error) {
	merged, _, err := review.WithDependencyWake(asBlock(out), prompt)
	if err != nil {
		return out, err
	}
	return a.continued(merged)
}

func (a *Adapter) RelayForDeveloper(
	out []byte,
	notice, markdown, before string,
) ([]byte, string, error) {
	merged, _, printed, err := review.WithDeveloperRelay(asBlock(out), notice, markdown, before)
	if err != nil {
		return out, "", err
	}
	merged, err = a.continued(merged)
	if err != nil {
		return out, "", err
	}
	return merged, printed, nil
}

func (*Adapter) DeliverNotice(
	message string,
) ([]byte, error) {
	return review.NoticeJSON(message)
}

func (*Adapter) DeliverPromptNotice(
	message, context string,
) ([]byte, error) {
	return json.Marshal(map[string]any{
		"injectSteps": []any{
			map[string]any{
				"systemMessage": map[string]any{
					"systemMessage": context,
					"metadata":      map[string]string{"user_markdown": message},
				},
			},
		},
	})
}

func surface(
	transcriptPath string,
) string {
	parts := strings.Split(filepath.ToSlash(transcriptPath), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.HasPrefix(parts[i], "antigravity") {
			return parts[i]
		}
	}
	return ""
}

func skippedTermination(
	reason string,
) bool {
	switch strings.ToUpper(reason) {
	case "USER_CANCELED", "ERROR", "MAX_FORCED_INVOCATIONS":
		return true
	}
	return false
}

func toolInput(
	args map[string]any,
) map[string]any {
	in := map[string]any{}
	for k, v := range args {
		in[k] = v
	}
	if s, ok := args["TargetFile"].(string); ok {
		in["file_path"] = s
	}
	if s, ok := args["CommandLine"].(string); ok {
		in["command"] = s
	}
	return in
}

func asBlock(
	out []byte,
) []byte {
	return withDecision(out, "continue", "block")
}

func (a *Adapter) continued(
	out []byte,
) ([]byte, error) {
	if err := a.markWoken(); err != nil {
		return nil, err
	}
	var hook struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out, &hook); err != nil {
		return nil, err
	}
	if hook.Decision == "block" {
		hook.Decision = "continue"
	}
	return json.Marshal(hook)
}

func withDecision(
	out []byte,
	from, to string,
) []byte {
	var hook map[string]json.RawMessage
	if json.Unmarshal(out, &hook) != nil {
		return out
	}
	var decision string
	if json.Unmarshal(hook["decision"], &decision) != nil || decision != from {
		return out
	}
	hook["decision"], _ = json.Marshal(to)
	rewritten, err := json.Marshal(hook)
	if err != nil {
		return out
	}
	return rewritten
}

type turnState struct {
	Prompt string `json:"prompt"`
	Ended  bool   `json:"ended"`
	Woken  bool   `json:"woken"`
}

func statePath(
	conversationID string,
) string {
	return filepath.Join(scratch.Dir("antigravity-turn"), fmt.Sprintf("%x", sha256.Sum256([]byte(conversationID))))
}

func readState(
	conversationID string,
) (turnState, bool) {
	var s turnState
	data, err := os.ReadFile(statePath(conversationID))
	if err != nil || json.Unmarshal(data, &s) != nil {
		return turnState{}, false
	}
	return s, true
}

func writeState(
	conversationID string,
	s turnState,
) error {
	path := statePath(conversationID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".turn-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func promptDigest(
	prompt string,
) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
}

func (a *Adapter) beginsTurn(
	prompt string,
) bool {
	if a.conversationID == "" {
		return true
	}
	s, ok := readState(a.conversationID)
	if ok && !s.Ended && s.Prompt == promptDigest(prompt) {
		return false
	}
	_ = writeState(a.conversationID, turnState{Prompt: promptDigest(prompt)})
	return true
}

func (a *Adapter) endTurn() bool {
	if a.conversationID == "" {
		return false
	}
	s, _ := readState(a.conversationID)
	woken := s.Woken
	s.Ended, s.Woken = true, false
	_ = writeState(a.conversationID, s)
	return woken
}

func (a *Adapter) markWoken() error {
	if a.conversationID == "" {
		return nil
	}
	s, _ := readState(a.conversationID)
	s.Ended, s.Woken = false, true
	return writeState(a.conversationID, s)
}
