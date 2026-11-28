package transcript

import (
	"strings"
	"testing"
	"time"
)

func TestReadGeminiTurnSkipsPriorTurnsAndDeduplicatesUpdates(t *testing.T) {
	path := writeTranscript(t,
		`{"sessionId":"session","startTime":"2026-11-24T20:00:00Z"}`,
		`{"id":"u1","type":"user","timestamp":"2026-11-24T20:00:00Z","content":[{"text":"Make the change"}]}`,
		`{"id":"g1","type":"gemini","content":"Original reply","tokens":{"input":50,"output":12}}`,
		`{"id":"u2","type":"user","timestamp":"2026-11-24T20:01:00Z","content":[{"text":"Apply the review feedback"}]}`,
		`{"id":"g2","type":"gemini","model":"gemini-4-neon","content":"Updating the code","tokens":{"input":20,"cached":5,"output":4,"thoughts":2}}`,
		`{"id":"g2","type":"gemini","model":"gemini-4-neon","content":"Updating the code","tokens":{"input":20,"cached":5,"output":4,"thoughts":2},"toolCalls":[]}`,
		`{"id":"tool","type":"user","content":[{"functionResponse":{"name":"write_file"}}]}`,
		`{"id":"g3","type":"gemini","model":"gemini-4-neon","content":"Updated","tokens":{"input":25,"cached":10,"output":3}}`,
	)
	got, err := ReadGeminiTurn(path, false)
	want := GeminiTurn{
		Model: "gemini-4-neon", PromptTime: time.Date(2026, 11, 24, 20, 1, 0, 0, time.UTC),
		InputTokens: 30, CacheReadTokens: 15, OutputTokens: 9, Reply: "Updating the code\n\nUpdated",
	}
	if err != nil || got != want {
		t.Fatalf("got %+v, want %+v, err=%v", got, want, err)
	}
	full, err := ReadGeminiTurn(path, true)
	want.PromptTime = time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	want.InputTokens += 50
	want.OutputTokens += 12
	want.InitialPrompt = "Make the change"
	if err != nil || full != want {
		t.Fatalf("full turn got %+v, want %+v, err=%v", full, want, err)
	}
}

func TestReadGeminiTurnStreamsPastTheOldCap(t *testing.T) {
	// Gemini rewrites each message as it grows; a long session passed the former 32 MiB cap.
	big := strings.Repeat("x", 1<<20)
	lines := []string{`{"id":"u1","type":"user","timestamp":"2026-11-24T20:00:00Z","content":[{"text":"Make the change"}]}`}
	for i := 0; i < 40; i++ {
		lines = append(lines, `{"id":"tool","type":"user","content":[{"functionResponse":{"name":"read_file","response":"`+big+`"}}]}`)
	}
	lines = append(lines, `{"id":"g1","type":"gemini","model":"gemini-4-neon","content":"Done","tokens":{"input":7,"output":3}}`)
	got, err := ReadGeminiTurn(writeTranscript(t, lines...), false)
	if err != nil || got.Model != "gemini-4-neon" || got.Reply != "Done" || got.OutputTokens != 3 {
		t.Fatalf("got %+v, err=%v", got, err)
	}
}
