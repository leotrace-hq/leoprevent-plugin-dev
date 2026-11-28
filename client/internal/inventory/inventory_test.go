package inventory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// Due is the check and the mark together, as a caller whose request succeeded does them.
func Due(agentName string, inv *wire.AgentInventory, now time.Time) bool {
	if !IsDue(agentName, inv, now) {
		return false
	}
	MarkSent(agentName, inv, now)
	return true
}

func snapshot(pluginVersion string) *wire.AgentInventory {
	return Normalize(&wire.AgentInventory{
		AccountType: wire.AccountCompany,
		Plugins:     []wire.InventoryPlugin{{Name: "leoprevent", Marketplace: "leotrace", Version: pluginVersion, Enabled: true}},
	})
}

// A settled session collects once, whatever the number of turns, and each agent and session is
// its own.
func TestCollectNowIsOncePerSession(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-time.Hour) // every session here started long ago
	if !CollectNow("claude", "s1", old, now) {
		t.Fatal("a settled session's first call must collect")
	}
	if CollectNow("claude", "s1", old, now.Add(time.Hour)) {
		t.Error("the same session must not collect twice")
	}
	if !CollectNow("claude", "s2", old, now) || !CollectNow("codex", "s1", old, now) {
		t.Error("another session, and another agent's session of the same id, each collect")
	}
	if CollectNow("claude", "", old, now) {
		t.Error("a turn with no session id cannot be deduplicated, so it must not collect")
	}
}

// ⚠️ A snapshot taken at a quick first Stop misses the connectors the agent is still attaching,
// so a young session waits: it collects on its first Stop settleAfter after it STARTED.
func TestCollectNowWaitsForTheSessionToSettle(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	start := time.Unix(1_800_000_000, 0)
	young, settled := settleAfter/2, settleAfter+time.Second

	// A two-second first turn: too young.
	if CollectNow("claude", "quick", start, start.Add(2*time.Second)) {
		t.Fatal("a session two seconds old must not collect")
	}
	// Its next Stop, a one-second turn, still too young. The turn's own start must not matter:
	// the session's start was fixed when it was first seen.
	if CollectNow("claude", "quick", start.Add(young-time.Second), start.Add(young)) {
		t.Error("a session that has not settled must not collect")
	}
	// A one-second turn once the session has settled: collected, though that turn is brand new.
	if !CollectNow("claude", "quick", start.Add(settled-time.Second), start.Add(settled)) {
		t.Error("a settled session must collect, however short its latest turn")
	}
	if CollectNow("claude", "quick", start, start.Add(time.Hour)) {
		t.Error("and then never again")
	}

	// One long first turn, the usual agentic session: settled at its first Stop.
	if !CollectNow("claude", "long", start, start.Add(10*time.Minute)) {
		t.Error("a first turn longer than the wait must collect at its first Stop")
	}

	// A turn whose start is unknown, or claims the future, counts from now.
	if CollectNow("claude", "unknown", time.Time{}, start) || CollectNow("claude", "future", start.Add(time.Hour), start) {
		t.Error("an unknown or future start must not look like an old session")
	}
	if !CollectNow("claude", "unknown", time.Time{}, start.Add(settled)) {
		t.Error("a session of unknown start collects once it has settled from when it was first seen")
	}
}

// The wait is a measured number (see settleAfter). Pinned so a change to it is a decision.
func TestSettleAfterIsThirtySeconds(t *testing.T) {
	if settleAfter != 30*time.Second {
		t.Errorf("settleAfter = %v, want 30s", settleAfter)
	}
}

// An unchanged install sends one snapshot a day. A changed one is sent at once.
func TestDueIsOncePerDayPerContent(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	now := time.Unix(1_800_000_000, 0)
	a := snapshot("0.2.48")
	if !Due("claude", a, now) {
		t.Fatal("a snapshot never sent is due")
	}
	if Due("claude", snapshot("0.2.48"), now.Add(23*time.Hour)) {
		t.Error("the same content within a day must not be sent again")
	}
	if !Due("claude", snapshot("0.2.49"), now.Add(time.Hour)) {
		t.Error("changed content is due at once")
	}
	if !Due("claude", snapshot("0.2.48"), now.Add(25*time.Hour)) {
		t.Error("the same content is due again after a day")
	}
	if Due("claude", nil, now) {
		t.Error("no snapshot is never due")
	}
}

// The order the config files were read in must not make an unchanged install look changed.
func TestHashIgnoresListOrder(t *testing.T) {
	a := Normalize(&wire.AgentInventory{
		MCPServers: []wire.InventoryMCPServer{{Name: "b", Scope: "user"}, {Name: "a", Scope: "project"}, {Name: "a", Scope: "local"}},
		Hooks:      []wire.InventoryHook{{Event: "Stop", Source: "user", Hash: "2"}, {Event: "Stop", Source: "user", Hash: "1"}},
	})
	b := Normalize(&wire.AgentInventory{
		MCPServers: []wire.InventoryMCPServer{{Name: "a", Scope: "local"}, {Name: "b", Scope: "user"}, {Name: "a", Scope: "project"}},
		Hooks:      []wire.InventoryHook{{Event: "Stop", Source: "user", Hash: "1"}, {Event: "Stop", Source: "user", Hash: "2"}},
	})
	if Hash(a) != Hash(b) {
		t.Error("the same items in another order must hash the same")
	}
	if Hash(a) == Hash(snapshot("1")) {
		t.Error("different content must hash differently")
	}
}

// ⚠️ The failure direction. A state file that cannot be read must stop collection and sending,
// not turn "once a day" into "every turn".
func TestStateErrorsMeanNoCollectionAndNoSend(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	now := time.Unix(1_800_000_000, 0)
	if err := os.MkdirAll(filepath.Dir(statePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if CollectNow("claude", "s1", now.Add(-time.Hour), now) {
		t.Error("a corrupt state file must not allow collection")
	}
	if Due("claude", snapshot("1"), now) {
		t.Error("a corrupt state file must not allow a send")
	}
}

// Old marks are dropped on write, so the state file stays small.
func TestStateIsPruned(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	start := time.Unix(1_800_000_000, 0)
	CollectNow("claude", "old", start.Add(-time.Hour), start)
	CollectNow("claude", "old-young", start, start) // seen, never settled
	Due("claude", snapshot("old"), start)
	later := start.Add(sessionKept + time.Hour)
	CollectNow("claude", "new", later.Add(-time.Hour), later)
	st, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := st.Sessions["claude/old"]; kept || len(st.Sessions) != 1 || len(st.Started) != 1 || len(st.Sent) != 0 {
		t.Errorf("stale marks must be pruned, got %+v", st)
	}
}

// A session that ends without having collected gets one snapshot at its end, but only when the
// machine sent none in the last day: a session that short can be short of connectors, and must
// not replace a full snapshot from the same day.
func TestCollectAtEndFillsInOnlyWhenNothingWasSentToday(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	now := time.Unix(1_800_000_000, 0)

	// Nothing sent yet: the ending session collects, once.
	if !CollectAtEnd("claude", "short-1", now) {
		t.Fatal("a session that ends uncollected on a machine that sent nothing must collect")
	}
	if CollectAtEnd("claude", "short-1", now) {
		t.Error("the same session must not collect twice at its end")
	}
	if CollectAtEnd("claude", "", now) {
		t.Error("an end with no session id must not collect")
	}

	// The machine now sends a snapshot. Until a day has passed no ending session collects.
	if !Due("claude", snapshot("0.2.48"), now) {
		t.Fatal("the first snapshot must be due")
	}
	if CollectAtEnd("claude", "short-2", now.Add(time.Hour)) {
		t.Error("an ending session must not collect when this machine sent a snapshot today")
	}
	// Another agent's snapshots are its own business.
	if !CollectAtEnd("codex", "short-2", now.Add(time.Hour)) {
		t.Error("another agent that sent nothing must still collect at its session's end")
	}
	// A day later the machine has reported nothing for a day again.
	if !CollectAtEnd("claude", "short-3", now.Add(25*time.Hour)) {
		t.Error("an ending session must collect once the last snapshot is over a day old")
	}
}

// A session that already collected at a Stop does not collect again when it ends, and one that
// collected at its end does not collect at a later Stop (a resumed session).
func TestCollectAtEndAndCollectNowShareTheSessionMark(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-time.Hour)
	if !CollectNow("claude", "s1", old, now) {
		t.Fatal("a settled session must collect at its Stop")
	}
	if CollectAtEnd("claude", "s1", now) {
		t.Error("a session that collected at a Stop must not collect again at its end")
	}
	if !CollectAtEnd("claude", "s2", now) {
		t.Fatal("an uncollected session must collect at its end")
	}
	if CollectNow("claude", "s2", old, now.Add(time.Minute)) {
		t.Error("a session that collected at its end must not collect at a later Stop")
	}
}
