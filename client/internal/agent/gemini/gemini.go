package gemini

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/review"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/transcript"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

type Adapter struct {
	sessionID   string
	prompt      string
	reviewRetry bool
	// originalPrompt is the developer's prompt for a turn LeoPrevent woke Gemini on. The
	// retry's own prompt is the review reason, which must never be reported as theirs.
	originalPrompt string
}

func New() *Adapter           { return &Adapter{} }
func (*Adapter) Name() string { return "gemini" }
func (*Adapter) Environment(agent.Event) agent.Environment {
	return agent.Environment{Name: wire.EnvGeminiCLI}
}

func (a *Adapter) ParseEvent(stdin []byte) (agent.Event, error) {
	var p struct {
		Name           string         `json:"hook_event_name"`
		SessionID      string         `json:"session_id"`
		Cwd            string         `json:"cwd"`
		TranscriptPath string         `json:"transcript_path"`
		Prompt         string         `json:"prompt"`
		Reply          string         `json:"prompt_response"`
		Active         bool           `json:"stop_hook_active"`
		Tool           string         `json:"tool_name"`
		Input          map[string]any `json:"tool_input"`
	}
	if err := json.Unmarshal(stdin, &p); err != nil {
		return agent.Event{}, err
	}
	a.sessionID, a.prompt, a.reviewRetry, a.originalPrompt = p.SessionID, p.Prompt, false, ""
	ev := agent.Event{SessionID: p.SessionID, Cwd: p.Cwd}
	if p.Name == "BeforeAgent" || p.Name == "AfterAgent" {
		ev.TranscriptPath = agent.ConfineTranscript(a.Name(), p.TranscriptPath, agent.GeminiTranscriptRoots())
	}
	switch p.Name {
	case "BeforeAgent":
		ev.Name = agent.EventUserPromptSubmit
		ev.ReviewContinuation = beginWake(p.SessionID, p.Prompt)
	case "BeforeTool":
		ev.Name = agent.EventPreToolUse
		ev.EditPaths = agent.EditPathsFromToolInput(p.Input)
		ev.TouchedPaths = agent.TouchedPathsFromToolInput(p.Input)
		if p.Tool == "run_shell_command" {
			dir, _ := p.Input["dir_path"].(string)
			for _, paths := range [][]string{ev.EditPaths, ev.TouchedPaths} {
				for i, path := range paths {
					if dir != "" && !filepath.IsAbs(path) {
						paths[i] = filepath.Join(dir, path)
					}
				}
			}
			// Edit discovery keeps only operands that look like paths, so `printf x > app.py`
			// run in dir_path names no edit at all and an undiscovered repository there would
			// get no baseline. The trailing separator makes discovery (which takes the parent
			// of an edit path) resolve to dir_path itself.
			if dir != "" {
				ev.EditPaths = append(ev.EditPaths, filepath.Clean(dir)+string(filepath.Separator))
			}
		}
	case "AfterAgent":
		ev.Name = "Stop"
		matched, original := finishWake(p.SessionID)
		// Gemini's own flag counts too: a retry whose wake file expired is still a retry, and
		// its usage and prompt still belong to the turn that was woken.
		a.reviewRetry = matched || p.Active
		a.originalPrompt = original
		ev.StopHookActive = a.reviewRetry
		// On retry Gemini returns the original reply and the repair reply together.
		// Read the repair reply from the transcript on that Stop.
		if !ev.StopHookActive {
			ev.LastAssistantMessage = p.Reply
		}
	default:
		return agent.Event{}, fmt.Errorf("unsupported Gemini event %q", p.Name)
	}
	return ev, nil
}

func (*Adapter) ChangedFiles(agent.Event) ([]transcript.Change, error) {
	return nil, nil
}

func (a *Adapter) TurnMeta(ev agent.Event) (agent.TurnMeta, error) {
	turn, err := transcript.ReadGeminiTurn(ev.TranscriptPath, a.reviewRetry)
	prompt := a.prompt
	if a.reviewRetry {
		// The retry's prompt is LeoPrevent's own review reason. Report the developer's
		// prompt (kept with the wake, else read from the recording) or none at all.
		prompt = a.originalPrompt
		if prompt == "" {
			prompt = turn.InitialPrompt
		}
	}
	meta := agent.TurnMeta{
		AgentModel:      turn.Model,
		Prompt:          prompt,
		PromptTime:      turn.PromptTime,
		InputTokens:     turn.InputTokens,
		CacheReadTokens: turn.CacheReadTokens,
		OutputTokens:    turn.OutputTokens,
	}
	return meta, err
}

func (*Adapter) AgentReply(ev agent.Event) (string, error) {
	turn, err := transcript.ReadGeminiTurn(ev.TranscriptPath, false)
	return turn.Reply, err
}

func (a *Adapter) DeliverReview(prompt, banner string, _ int, _ []wire.Finding, _ string) ([]byte, error) {
	out, err := review.RewakeJSON(prompt, banner)
	if err != nil {
		return nil, err
	}
	// A failed write only costs the retry its preserved baseline. Returning the error would
	// make the engine fail open and drop the review, so a found flaw would never be fixed.
	if err := a.rememberWake(prompt); err != nil {
		slog.Warn("gemini: retry marker not saved; the fix turn will take a new baseline", "err", err.Error())
	}
	return out, nil
}

func (a *Adapter) WakeForDependencies(out []byte, prompt string) ([]byte, error) {
	merged, reason, err := review.WithDependencyWake(out, prompt)
	if err != nil {
		return out, err
	}
	if err := a.rememberWake(reason); err != nil {
		slog.Warn("gemini: retry marker not saved; the fix turn will take a new baseline", "err", err.Error())
	}
	return merged, nil
}

func (*Adapter) DeliverNotice(message string) ([]byte, error) {
	return review.NoticeJSON(message)
}

func (*Adapter) DeliverPromptNotice(message, context string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"systemMessage": message,
		"hookSpecificOutput": map[string]string{
			"hookEventName":     "BeforeAgent",
			"additionalContext": context,
		},
	})
}

func wakePath(sessionID string) string {
	return filepath.Join(scratch.Dir("gemini-guard"), fmt.Sprintf("%x", sha256.Sum256([]byte(sessionID))))
}

func wakeDigest(prompt string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
}

// BeforeAgent runs again before Gemini submits the review reason to the model and
// supplies no retry flag. Preserve the original baseline only for our exact reason.
func (a *Adapter) rememberWake(reason string) error {
	if a.sessionID == "" {
		return fmt.Errorf("Gemini session_id is missing")
	}
	path := wakePath(a.sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// The developer's prompt goes first: a digest without it would make the retry report none.
	if err := writeAtomic(path+".prompt", a.prompt); err != nil {
		return err
	}
	return writeAtomic(path, wakeDigest(reason))
}

func writeAtomic(path, data string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".wake-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func beginWake(sessionID, prompt string) bool {
	if sessionID == "" {
		return false
	}
	path := wakePath(sessionID)
	_ = os.Remove(path + ".active")
	_ = os.Remove(path + ".active.prompt")
	fi, err := os.Lstat(path)
	if err != nil {
		_ = os.Remove(path + ".prompt")
		return false
	}
	defer os.Remove(path)
	defer os.Remove(path + ".prompt")
	if !fi.Mode().IsRegular() || fi.Size() != sha256.Size*2 || time.Since(fi.ModTime()) > 10*time.Minute {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != wakeDigest(prompt) || os.Rename(path, path+".active") != nil {
		return false
	}
	_ = os.Rename(path+".prompt", path+".active.prompt")
	return true
}

// In headless mode a retry that uses a tool can reach AfterAgent without Gemini's
// stop_hook_active bit. A matched BeforeAgent still identifies it as our retry.
// It also returns the developer's prompt saved with the wake ("" when there is none).
func finishWake(sessionID string) (bool, string) {
	if sessionID == "" {
		return false, ""
	}
	path := wakePath(sessionID) + ".active"
	var prompt string
	if pfi, err := os.Lstat(path + ".prompt"); err == nil && pfi.Mode().IsRegular() && pfi.Size() <= 1<<20 {
		if data, err := os.ReadFile(path + ".prompt"); err == nil {
			prompt = string(data)
		}
	}
	_ = os.Remove(path + ".prompt")
	fi, err := os.Lstat(path)
	if err != nil {
		return false, ""
	}
	if os.Remove(path) != nil || !fi.Mode().IsRegular() || time.Since(fi.ModTime()) > 6*time.Hour {
		return false, ""
	}
	return true, prompt
}
