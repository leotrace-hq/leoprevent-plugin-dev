package engine

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent/claude"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/inventory"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/policycheck"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/transcript"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// inventoryReviewer is a reviewer with the agent inventory switched on or off, as the cloud
// tier's is by the client's `agent_inventory` setting. A plain fakeReviewer has no such method,
// which the engine reads as off.
type inventoryReviewer struct {
	*fakeReviewer
	on bool
	// sent holds every snapshot request the engine handed over, in order.
	sent *[]wire.InventoryRequest
}

func newInventoryReviewer(on bool) inventoryReviewer {
	return inventoryReviewer{fakeReviewer: &fakeReviewer{}, on: on, sent: new([]wire.InventoryRequest)}
}

func (r inventoryReviewer) AgentInventoryEnabled() bool { return r.on }

func (r inventoryReviewer) ShipInventory(req wire.InventoryRequest) error {
	*r.sent = append(*r.sent, req)
	return nil
}

// claudeInstall points the Claude adapter at an empty config directory holding one enabled
// plugin, so the snapshot is the same on every machine the test runs on.
func claudeInstall(t *testing.T) {
	t.Helper()
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	// These transcripts carry no timestamps, so every session would look brand new. The wait for
	// a session to settle has its own test below.
	t.Cleanup(inventory.SetSettleAfterForTest(0))
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, k := range []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(k, "")
	}
	files := map[string]string{
		"settings.json":                  `{"enabledPlugins": {"leoprevent@leotrace": true}}`,
		"plugins/installed_plugins.json": `{"plugins": {"leoprevent@leotrace": [{"scope": "user", "version": "0.2.48"}]}}`,
	}
	for rel, content := range files {
		p := filepath.Join(cfg, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func sessionPayload(session, transcriptPath string) string {
	return fmt.Sprintf(`{"stop_hook_active":false,"session_id":%q,"transcript_path":%q,"cwd":""}`, session, transcriptPath)
}

func chatTranscript(t *testing.T) string {
	t.Helper()
	return writeTranscript(t,
		`{"type":"user","version":"2.1.317","permissionMode":"auto","message":{"role":"user","content":"what is in the ticket?"}}`,
		`{"type":"assistant","version":"2.1.317","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","name":"mcp__claude_ai_Linear__get_issue","input":{}},{"type":"tool_use","name":"Skill","input":{"skill":"design-system"}}]}}`,
	)
}

// The per-turn half rides every turn's metadata. The snapshot goes out on a request of its own,
// one per session, and an unchanged install does not send it again within the day, from this
// session or the next.
func TestInventoryIsSentOncePerSessionOnItsOwnRequest(t *testing.T) {
	claudeInstall(t)
	tp := chatTranscript(t)

	r := newInventoryReviewer(true)
	runWith(claude.New(), r, sessionPayload("inv-s1", tp))
	if r.telemetryCalls != 1 {
		t.Fatalf("expected one telemetry call, got %d", r.telemetryCalls)
	}
	m := r.telemetryMeta
	if m.AgentVersion != "2.1.317" || m.PermissionMode != "auto" || m.APIEndpoint != "anthropic" {
		t.Errorf("per-turn inventory = %q/%q/%q, want 2.1.317/auto/anthropic", m.AgentVersion, m.PermissionMode, m.APIEndpoint)
	}
	if want := []string{"claude_ai_Linear"}; !reflect.DeepEqual(m.MCPServersUsed, want) {
		t.Errorf("servers used = %q, want %q", m.MCPServersUsed, want)
	}
	if want := []string{"design-system"}; !reflect.DeepEqual(m.SkillsUsed, want) {
		t.Errorf("skills used = %q, want %q", m.SkillsUsed, want)
	}
	installID := m.InstallID
	if wire.ValidInstallID(installID) == "" {
		t.Errorf("install id = %q, want this installation's id on the turn", installID)
	}
	if len(*r.sent) != 1 {
		t.Fatalf("the session's first turn must send one snapshot, sent %d", len(*r.sent))
	}
	req := (*r.sent)[0]
	// The request names the installation the turn named, and carries the facts that place it.
	if req.InstallID != installID || req.Agent != "claude" || req.AgentVersion != "2.1.317" ||
		req.OS != m.OS || req.Arch != m.Arch || req.ClientVersion != m.ClientVersion || req.Developer != m.Developer {
		t.Errorf("snapshot request = %+v, want it to name the turn's installation %q", req, installID)
	}
	if req.Inventory == nil || len(req.Inventory.Plugins) != 1 || req.Inventory.Plugins[0].Name != "leoprevent" || !req.Inventory.Plugins[0].Enabled {
		t.Errorf("snapshot plugins = %+v", req.Inventory)
	}
	if len(req.Inventory.Connectors) != 1 || req.Inventory.Connectors[0].Name != "Linear" {
		t.Errorf("snapshot connectors = %+v, want the connector the session used", req.Inventory.Connectors)
	}

	// Same session, next turn: per-turn fields again, no snapshot.
	runWith(claude.New(), r, sessionPayload("inv-s1", tp))
	if r.telemetryCalls != 2 || len(*r.sent) != 1 {
		t.Errorf("a session's later turn must not resend the snapshot (telemetry=%d snapshots=%d)", r.telemetryCalls, len(*r.sent))
	}
	if r.telemetryMeta.AgentVersion != "2.1.317" {
		t.Error("the per-turn half must ride every turn")
	}
	if r.telemetryMeta.InstallID != installID {
		t.Errorf("install id changed within a session: %q then %q", installID, r.telemetryMeta.InstallID)
	}

	// A new session collects again, finds the install unchanged, and stays quiet.
	runWith(claude.New(), r, sessionPayload("inv-s2", tp))
	if len(*r.sent) != 1 {
		t.Errorf("an unchanged install must not be resent within the day, sent %d", len(*r.sent))
	}
	// The id is the installation's, not the session's: it is what ties the two together.
	if r.telemetryMeta.InstallID != installID {
		t.Errorf("install id changed between sessions: %q then %q", installID, r.telemetryMeta.InstallID)
	}
}

// A reviewed turn sends the snapshot on its own request too, beside the review. The review
// request carries the per-turn half only.
func TestInventoryIsSentBesideAReview(t *testing.T) {
	claudeInstall(t)
	tp := writeTranscript(t,
		`{"type":"user","version":"2.1.317","permissionMode":"acceptEdits","message":{"role":"user","content":"add a refresh route"}}`,
		`{"type":"assistant","version":"2.1.317","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/p/app.py","content":"url = request.args['url']\nreturn requests.get(url).text"}}]}}`,
	)
	r := newInventoryReviewer(true)
	runWith(claude.New(), r, sessionPayload("inv-review", tp))
	if !r.called || r.telemetryCalls != 0 {
		t.Fatalf("expected a review and no telemetry, called=%v telemetry=%d", r.called, r.telemetryCalls)
	}
	if r.gotMeta.PermissionMode != "acceptEdits" || r.gotMeta.AgentVersion != "2.1.317" {
		t.Errorf("review meta mode=%q version=%q", r.gotMeta.PermissionMode, r.gotMeta.AgentVersion)
	}
	if len(*r.sent) != 1 || (*r.sent)[0].Inventory == nil || (*r.sent)[0].InstallID != r.gotMeta.InstallID {
		t.Errorf("snapshot requests = %+v, want one naming the reviewed turn's installation", *r.sent)
	}
}

// Switched off, or on a reviewer that has no such switch, NONE of it is sent: not the snapshot
// and not the per-turn half.
func TestInventoryOffSendsNothing(t *testing.T) {
	claudeInstall(t)
	tp := chatTranscript(t)
	off := newInventoryReviewer(false)
	for name, r := range map[string]Reviewer{
		"switched off":      off,
		"reviewer has none": &fakeReviewer{},
	} {
		runWith(claude.New(), r, sessionPayload("inv-off-"+name, tp))
		var m wire.TurnMeta
		switch v := r.(type) {
		case inventoryReviewer:
			m = v.telemetryMeta
		case *fakeReviewer:
			m = v.telemetryMeta
		}
		if m.InstallID != "" || m.AgentVersion != "" || m.PermissionMode != "" || m.APIEndpoint != "" || m.MCPServersUsed != nil || m.SkillsUsed != nil {
			t.Errorf("%s: inventory fields were sent: %+v", name, m)
		}
		// The rest of the turn's metadata is untouched.
		if m.Agent != "claude" {
			t.Errorf("%s: turn meta lost its agent: %+v", name, m)
		}
	}
	if len(*off.sent) != 0 {
		t.Errorf("switched off, a snapshot was sent: %+v", *off.sent)
	}
}

// policyOnlyReviewer has the inventory switched on and no way to send a snapshot.
type policyOnlyReviewer struct{ *fakeReviewer }

func (policyOnlyReviewer) AgentInventoryEnabled() bool { return true }

// A reviewer that cannot send a snapshot must not use up the session's one snapshot: the
// session is left uncollected, so a reviewer that can send still sends it.
func TestInventoryIsNotCollectedForAReviewerThatCannotSendIt(t *testing.T) {
	claudeInstall(t)
	tp := chatTranscript(t)
	runWith(claude.New(), policyOnlyReviewer{&fakeReviewer{}}, sessionPayload("inv-no-shipper", tp))
	r := newInventoryReviewer(true)
	runWith(claude.New(), r, sessionPayload("inv-no-shipper", tp))
	if len(*r.sent) != 1 {
		t.Errorf("the session's snapshot was used up by a reviewer that could not send it, sent %d", len(*r.sent))
	}
}

// An adapter that cannot describe its install sends none, with the switch on.
func TestInventoryNeedsAnAdapterThatReportsIt(t *testing.T) {
	t.Cleanup(scratch.SetUserCacheDirForTest(t.TempDir()))
	r := newInventoryReviewer(true)
	shipInventory(wire.TurnMeta{}, noticeAgent{}, r, agent.Event{SessionID: "inv-no-adapter"}, slog.Default(), nowFunc())
	if len(*r.sent) != 0 {
		t.Error("an adapter without agent.Inventorier must send no snapshot")
	}
	// Nor the installation's id: with no inventory there is nothing for it to key.
	if got := turnMeta(noticeAgent{}, r, agent.Event{SessionID: "inv-no-adapter"}, slog.Default(), nowFunc()); got.InstallID != "" {
		t.Errorf("an adapter without agent.Inventorier sent the install id %q", got.InstallID)
	}
}

// ⚠️ A quick first Stop comes before the agent has attached its connectors, so a young session
// sends no snapshot. It is sent on the session's first Stop after the session has settled
// instead. The per-turn half is not held back.
func TestInventoryWaitsForTheSessionToSettle(t *testing.T) {
	claudeInstall(t)
	t.Cleanup(inventory.SetSettleAfterForTest(time.Minute))
	tp := chatTranscript(t)
	start := time.Unix(1_800_000_000, 0)
	prev := nowFunc
	t.Cleanup(func() { nowFunc = prev })

	r := newInventoryReviewer(true)
	nowFunc = func() time.Time { return start.Add(2 * time.Second) }
	runWith(claude.New(), r, sessionPayload("inv-young", tp))
	if r.telemetryCalls != 1 || len(*r.sent) != 0 {
		t.Fatalf("a session two seconds old must send no snapshot (telemetry=%d snapshots=%d)", r.telemetryCalls, len(*r.sent))
	}
	if r.telemetryMeta.AgentVersion != "2.1.317" || wire.ValidInstallID(r.telemetryMeta.InstallID) == "" {
		t.Error("the per-turn half must not wait for the session to settle")
	}

	nowFunc = func() time.Time { return start.Add(90 * time.Second) }
	runWith(claude.New(), r, sessionPayload("inv-young", tp))
	if r.telemetryCalls != 2 || len(*r.sent) != 1 {
		t.Fatalf("the session's first Stop after it settled must send the snapshot (telemetry=%d snapshots=%d)", r.telemetryCalls, len(*r.sent))
	}
}

// besideReviewer proves ordering: its snapshot request does not return until the review has
// started, and its review does not return until the snapshot has arrived.
type besideReviewer struct {
	inventoryReviewer
	reviewStarted   chan struct{}
	snapshotArrived chan struct{}
	duringReview    *bool
}

func (r besideReviewer) Review(cwd string, ch []transcript.Change, meta wire.TurnMeta, id string) (Result, error) {
	close(r.reviewStarted)
	select {
	case <-r.snapshotArrived:
	case <-time.After(5 * time.Second):
	}
	return r.fakeReviewer.Review(cwd, ch, meta, id)
}

func (r besideReviewer) ShipInventory(req wire.InventoryRequest) error {
	select {
	case <-r.reviewStarted:
		*r.duringReview = true
	case <-time.After(5 * time.Second):
	}
	close(r.snapshotArrived)
	return r.inventoryReviewer.ShipInventory(req)
}

// ⚠️ The snapshot must not hold up the review. It is sent while the review request is in flight,
// not before it: sent first, the developer would wait for it on top of the review.
func TestInventoryIsSentWhileTheReviewIsInFlight(t *testing.T) {
	claudeInstall(t)
	tp := writeTranscript(t,
		`{"type":"user","version":"2.1.317","permissionMode":"acceptEdits","message":{"role":"user","content":"add a refresh route"}}`,
		`{"type":"assistant","version":"2.1.317","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"/p/app.py","content":"url = request.args['url']\nreturn requests.get(url).text"}}]}}`,
	)
	during := false
	r := besideReviewer{inventoryReviewer: newInventoryReviewer(true),
		reviewStarted: make(chan struct{}), snapshotArrived: make(chan struct{}), duringReview: &during}
	runWith(claude.New(), r, sessionPayload("inv-beside", tp))
	if !r.called {
		t.Fatal("expected a review")
	}
	if len(*r.sent) != 1 {
		t.Fatalf("the snapshot must be sent by the time the hook returns, sent %d", len(*r.sent))
	}
	if !during {
		t.Error("the snapshot request was not in flight during the review: it was sent before the review started")
	}
}

// panicInventoryAgent is Claude's adapter with a snapshot reader that panics.
type panicInventoryAgent struct{ *claude.Adapter }

func (panicInventoryAgent) Inventory(agent.Event) (*wire.AgentInventory, error) {
	panic("a configuration file nobody expected")
}

// A panic while the snapshot is read happens on its own goroutine, where main's backstop does
// not reach. It must end there: the hook still exits 0 and the turn's own request still goes out.
func TestInventoryPanicDoesNotFailTheStop(t *testing.T) {
	claudeInstall(t)
	r := newInventoryReviewer(true)
	code, _, _ := runWith(panicInventoryAgent{claude.New()}, r, sessionPayload("inv-panic", chatTranscript(t)))
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (fail open)", code)
	}
	if r.telemetryCalls != 1 {
		t.Errorf("the turn's telemetry must still be sent, calls=%d", r.telemetryCalls)
	}
	if len(*r.sent) != 0 {
		t.Errorf("a snapshot was sent from a reader that panicked: %+v", *r.sent)
	}
}

// stuckReviewer's snapshot request never returns until release is closed. It then fails, and
// says on finished that it is done, so the test can wait for the goroutine before it ends.
type stuckReviewer struct {
	inventoryReviewer
	release  chan struct{}
	finished chan struct{}
}

func (r stuckReviewer) ShipInventory(wire.InventoryRequest) error {
	defer close(r.finished)
	<-r.release
	return fmt.Errorf("released")
}

// A snapshot that is stuck does not hold the hook: after inventoryWait the hook stops waiting.
func TestStuckInventoryDoesNotHoldTheHook(t *testing.T) {
	claudeInstall(t)
	prev := inventoryWait
	inventoryWait = 50 * time.Millisecond
	t.Cleanup(func() { inventoryWait = prev })
	r := stuckReviewer{inventoryReviewer: newInventoryReviewer(true), release: make(chan struct{}), finished: make(chan struct{})}
	t.Cleanup(func() { close(r.release); <-r.finished; time.Sleep(20 * time.Millisecond) })

	start := time.Now()
	code, _, _ := runWith(claude.New(), r, sessionPayload("inv-stuck", chatTranscript(t)))
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("the hook waited %v for a stuck snapshot, want about %v", waited, inventoryWait)
	}
	if r.telemetryCalls != 1 {
		t.Errorf("the turn's telemetry must still be sent, calls=%d", r.telemetryCalls)
	}
}

func sessionEndWith(a agent.Agent, r Reviewer, session, transcriptPath string) {
	SessionEnd(a, r, agent.Event{Name: agent.EventSessionEnd, SessionID: session, TranscriptPath: transcriptPath})
}

// A session that ended before it settled sent no snapshot at its Stop. Its end sends it, on the
// snapshot's own request, with no review and no telemetry.
func TestSessionEndSendsTheSnapshotAShortSessionNeverSent(t *testing.T) {
	claudeInstall(t)
	t.Cleanup(inventory.SetSettleAfterForTest(time.Minute))
	tp := chatTranscript(t)
	start := time.Unix(1_800_000_000, 0)
	prev := nowFunc
	t.Cleanup(func() { nowFunc = prev })
	nowFunc = func() time.Time { return start.Add(2 * time.Second) }

	r := newInventoryReviewer(true)
	runWith(claude.New(), r, sessionPayload("inv-short", tp))
	if len(*r.sent) != 0 {
		t.Fatalf("a session two seconds old must send no snapshot at its Stop, sent %d", len(*r.sent))
	}

	nowFunc = func() time.Time { return start.Add(5 * time.Second) }
	sessionEndWith(claude.New(), r, "inv-short", tp)
	if len(*r.sent) != 1 {
		t.Fatalf("the session's end must send its snapshot, sent %d", len(*r.sent))
	}
	req := (*r.sent)[0]
	if wire.ValidInstallID(req.InstallID) == "" || req.Agent != "claude" || req.AgentVersion != "2.1.317" || req.Inventory == nil {
		t.Errorf("snapshot request = %+v, want it to name the installation and carry the snapshot", req)
	}
	// It is not a Stop: the one telemetry call is the Stop's, and nothing was reviewed.
	if r.called || r.telemetryCalls != 1 {
		t.Errorf("the session's end reviewed or sent telemetry: called=%v telemetry=%d", r.called, r.telemetryCalls)
	}

	// The end of the same session again (a resume that ends) sends nothing more.
	sessionEndWith(claude.New(), r, "inv-short", tp)
	if len(*r.sent) != 1 {
		t.Errorf("a session's end must send at most one snapshot, sent %d", len(*r.sent))
	}
}

// ⚠️ A short session's snapshot can be short of connectors. Where the machine already sent a
// snapshot today, the end of a short session sends none, so it cannot replace the full one.
func TestSessionEndSendsNothingWhenTheMachineReportedToday(t *testing.T) {
	claudeInstall(t)
	tp := chatTranscript(t)
	r := newInventoryReviewer(true)
	runWith(claude.New(), r, sessionPayload("inv-long", tp)) // settled (the wait is 0 here): sends
	if len(*r.sent) != 1 {
		t.Fatalf("expected the settled session to send one snapshot, sent %d", len(*r.sent))
	}
	sessionEndWith(claude.New(), r, "inv-long", tp)  // already collected
	sessionEndWith(claude.New(), r, "inv-other", tp) // never collected, but the machine reported today
	if len(*r.sent) != 1 {
		t.Errorf("a session's end sent a snapshot on a machine that already reported today, sent %d", len(*r.sent))
	}
}

// Switched off, on an adapter that reports none, or on a reviewer that cannot send one, the end
// of a session does nothing at all.
func TestSessionEndOffSendsNothing(t *testing.T) {
	claudeInstall(t)
	tp := chatTranscript(t)
	off := newInventoryReviewer(false)
	sessionEndWith(claude.New(), off, "inv-end-off", tp)
	on := newInventoryReviewer(true)
	sessionEndWith(noticeAgent{}, on, "inv-end-no-adapter", tp)
	sessionEndWith(claude.New(), policyOnlyReviewer{&fakeReviewer{}}, "inv-end-no-shipper", tp)
	if len(*off.sent) != 0 || len(*on.sent) != 0 {
		t.Errorf("a snapshot was sent: off=%d adapter-without-inventory=%d", len(*off.sent), len(*on.sent))
	}
	// The reviewer that could not send did not use up the session's snapshot.
	sessionEndWith(claude.New(), on, "inv-end-no-shipper", tp)
	if len(*on.sent) != 1 {
		t.Errorf("the session's snapshot was used up by a reviewer that could not send it, sent %d", len(*on.sent))
	}
}

// refusingReviewer's snapshot request is refused while fail is set.
type refusingReviewer struct {
	inventoryReviewer
	fail *bool
}

func (r refusingReviewer) ShipInventory(req wire.InventoryRequest) error {
	if *r.fail {
		return fmt.Errorf("status 404")
	}
	return r.inventoryReviewer.ShipInventory(req)
}

// A snapshot the server did not accept is not recorded as sent: the next session sends it.
func TestRefusedInventoryIsSentAgainByTheNextSession(t *testing.T) {
	claudeInstall(t)
	tp := chatTranscript(t)
	fail := true
	r := refusingReviewer{inventoryReviewer: newInventoryReviewer(true), fail: &fail}
	runWith(claude.New(), r, sessionPayload("inv-refused-1", tp))
	if len(*r.sent) != 0 {
		t.Fatalf("sent %d, want none while the server refuses", len(*r.sent))
	}
	fail = false
	runWith(claude.New(), r, sessionPayload("inv-refused-2", tp))
	if len(*r.sent) != 1 {
		t.Errorf("the next session must send the snapshot the server refused, sent %d", len(*r.sent))
	}
}

// A session kept open for days sends one snapshot a day, not one in all.
func TestLongSessionSendsOnceADay(t *testing.T) {
	claudeInstall(t)
	tp := chatTranscript(t)
	start := time.Unix(1_800_000_000, 0)
	prev := nowFunc
	t.Cleanup(func() { nowFunc = prev })
	r := newInventoryReviewer(true)
	for i, at := range []time.Duration{time.Minute, 2 * time.Hour, 25 * time.Hour, 26 * time.Hour} {
		nowFunc = func() time.Time { return start.Add(at) }
		runWith(claude.New(), r, sessionPayload("inv-days", tp))
		if want := []int{1, 1, 2, 2}[i]; len(*r.sent) != want {
			t.Fatalf("after %v: sent %d, want %d", at, len(*r.sent), want)
		}
	}
}

// ⚠️ A session with no transcript sends no snapshot, from a Stop or from its end. The connectors
// and the agent's version come from the transcript, so its snapshot would list none: live, a
// VS Code tab that was opened and never prompted sent "0 connectors" for an install that has
// twelve when the editor closed. It is not marked either, so the session still sends once it
// has a transcript.
func TestSessionWithoutATranscriptSendsNoSnapshot(t *testing.T) {
	claudeInstall(t)
	r := newInventoryReviewer(true)

	sessionEndWith(claude.New(), r, "inv-no-transcript", "")
	if len(*r.sent) != 0 {
		t.Fatalf("the end of a session with no transcript sent a snapshot: %+v", *r.sent)
	}
	runWith(claude.New(), r, sessionPayload("inv-no-transcript", ""))
	if len(*r.sent) != 0 {
		t.Fatalf("a Stop with no transcript sent a snapshot: %+v", *r.sent)
	}

	// The same session with a transcript still sends: nothing above used up its snapshot.
	runWith(claude.New(), r, sessionPayload("inv-no-transcript", chatTranscript(t)))
	if len(*r.sent) != 1 || len((*r.sent)[0].Inventory.Connectors) != 1 {
		t.Errorf("the session must send once it has a transcript, sent %+v", *r.sent)
	}
}

// accountReviewer is the cloud tier as the engine sees it: it can send a snapshot, the client
// has not switched the inventory off, and its server answers the policy check with the
// account's switch.
type accountReviewer struct {
	inventoryReviewer
	accountOn bool
	asked     *int
}

func (r accountReviewer) PolicyCheck(string) (policycheck.Answer, error) {
	*r.asked++
	return policycheck.Answer{AgentInventory: r.accountOn}, nil
}

func newAccountReviewer(accountOn bool) accountReviewer {
	return accountReviewer{inventoryReviewer: newInventoryReviewer(true), accountOn: accountOn, asked: new(int)}
}

// realAccountSwitch puts the real reader of the account's switch back for one test (TestMain
// turns it on for the rest).
func realAccountSwitch(t *testing.T) {
	t.Helper()
	prev := accountInventoryOn
	accountInventoryOn = func() bool { return policycheck.AgentInventoryOn(licenseKey()) }
	t.Cleanup(func() { accountInventoryOn = prev })
}

// ⚠️ THE ACCOUNT'S SWITCH (`feature_agent_inventory`) IS OFF UNLESS STAFF TURNED IT ON. For an
// account without it nothing of the inventory leaves the computer: no snapshot on a turn, none
// at the end of a session, and none of the per-turn fields, although the client's own setting
// is on.
func TestInventoryNeedsTheAccountSwitchedOn(t *testing.T) {
	claudeInstall(t)
	realAccountSwitch(t)
	tp := chatTranscript(t)

	off := newAccountReviewer(false)
	runWith(claude.New(), off, sessionPayload("inv-acct-off", tp))
	runWith(claude.New(), off, sessionPayload("inv-acct-off", tp))
	sessionEndWith(claude.New(), off, "inv-acct-off-end", tp)
	if len(*off.sent) != 0 {
		t.Fatalf("account off, a snapshot was sent: %+v", *off.sent)
	}
	if m := off.telemetryMeta; m.InstallID != "" || m.AgentVersion != "" || m.PermissionMode != "" || m.APIEndpoint != "" || m.MCPServersUsed != nil || m.SkillsUsed != nil {
		t.Errorf("account off, inventory fields were sent on the turn: %+v", m)
	}
	if *off.asked == 0 {
		t.Error("the switch is learned from the policy check, which was never asked")
	}
}

// Switched on, the end of a session that never reached a Stop still sends its snapshot: the
// policy check it makes is what tells it the account is on.
func TestSessionEndLearnsTheAccountSwitchFromItsOwnCheck(t *testing.T) {
	claudeInstall(t)
	realAccountSwitch(t)
	tp := chatTranscript(t)
	on := newAccountReviewer(true)
	sessionEndWith(claude.New(), on, "inv-acct-on-end", tp)
	if len(*on.sent) != 1 {
		t.Fatalf("account on, sent %d snapshots at the end of the session, want 1", len(*on.sent))
	}
}
