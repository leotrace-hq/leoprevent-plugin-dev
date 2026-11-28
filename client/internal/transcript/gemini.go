package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// geminiTranscriptCap bounds how much of a recording is read. Gemini CLI rewrites every
// message (tool output included) several times, so a long session's file grows fast: 776
// A.S.E cells measured a 0.6 MiB median and 18 MiB maximum. Lines are streamed, so the
// cap bounds read time, not memory.
const geminiTranscriptCap = 512 << 20

type GeminiTurn struct {
	Model           string
	PromptTime      time.Time
	InputTokens     int
	CacheReadTokens int
	OutputTokens    int
	Reply           string
	// InitialPrompt is the user text of the turn before the latest one, read only for a
	// confirmed review retry: the developer's prompt, where the latest is the review reason.
	InitialPrompt string
}

type geminiMessage struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Content   json.RawMessage `json:"content"`
	Model     string          `json:"model"`
	Tokens    struct {
		Input   int `json:"input"`
		Cached  int `json:"cached"`
		Output  int `json:"output"`
		Thought int `json:"thoughts"`
	} `json:"tokens"`
}

// ReadGeminiTurn reads the latest user turn from Gemini CLI's JSONL recording.
// The CLI appends updated copies of a message using the same id. Tool responses
// are synthetic user messages whose content has no user text. For a confirmed review
// retry, usage and timing include the initial turn, while Reply covers only the retry.
func ReadGeminiTurn(path string, includeInitialTurn bool) (GeminiTurn, error) {
	if path == "" {
		return GeminiTurn{}, nil
	}
	f, err := openTranscript(path)
	if err != nil {
		return GeminiTurn{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return GeminiTurn{}, err
	}
	if info.Size() > geminiTranscriptCap {
		return GeminiTurn{}, fmt.Errorf("Gemini transcript exceeds %d MiB", geminiTranscriptCap>>20)
	}
	var turn, initial GeminiTurn
	var promptID, promptText string
	var order []string
	messages := make(map[string]geminiMessage)
	br := bufio.NewReader(io.LimitReader(f, geminiTranscriptCap))
	for {
		raw, readErr := br.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return GeminiTurn{}, readErr
		}
		var message geminiMessage
		if len(strings.TrimSpace(string(raw))) == 0 || json.Unmarshal(raw, &message) != nil || message.ID == "" {
			if readErr == io.EOF {
				break
			}
			continue
		}
		switch message.Type {
		case "user":
			if text := geminiText(message.Content); message.ID != promptID && text != "" {
				if includeInitialTurn {
					initial = summarizeGeminiTurn(turn, order, messages)
					initial.InitialPrompt = promptText
				}
				promptID, promptText = message.ID, text
				turn = GeminiTurn{PromptTime: message.Timestamp}
				order = order[:0]
				clear(messages)
			}
		case "gemini":
			if _, seen := messages[message.ID]; !seen {
				order = append(order, message.ID)
			}
			messages[message.ID] = message
		}
		if readErr == io.EOF {
			break
		}
	}
	turn = summarizeGeminiTurn(turn, order, messages)
	if includeInitialTurn && !initial.PromptTime.IsZero() {
		turn.InitialPrompt = initial.InitialPrompt
		turn.PromptTime = initial.PromptTime
		turn.InputTokens += initial.InputTokens
		turn.CacheReadTokens += initial.CacheReadTokens
		turn.OutputTokens += initial.OutputTokens
	}
	return turn, nil
}

func summarizeGeminiTurn(turn GeminiTurn, order []string, messages map[string]geminiMessage) GeminiTurn {
	var replies []string
	for _, id := range order {
		message := messages[id]
		if message.Model != "" {
			turn.Model = message.Model
		}
		turn.InputTokens += max(0, message.Tokens.Input-message.Tokens.Cached)
		turn.CacheReadTokens += message.Tokens.Cached
		turn.OutputTokens += message.Tokens.Output + message.Tokens.Thought
		if text := geminiText(message.Content); text != "" {
			replies = append(replies, text)
		}
	}
	turn.Reply = strings.Join(replies, "\n\n")
	return turn
}

func geminiText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return strings.TrimSpace(text)
	}
	var parts []struct {
		Text    string `json:"text"`
		Thought bool   `json:"thought"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var texts []string
	for _, part := range parts {
		if part.Text != "" && !part.Thought {
			texts = append(texts, part.Text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}
