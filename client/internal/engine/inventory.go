package engine

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/agent"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/inventory"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/policycheck"
	"github.com/leotrace-hq/leoprevent-plugin/client/internal/vcs"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// inventoryPolicy is the optional half of a Reviewer that says whether the agent inventory
// (LEO-822) is sent. Found by asking, so a reviewer without it sends none: the cloud tier has it,
// and it reports the client's `agent_inventory` setting.
type inventoryPolicy interface {
	AgentInventoryEnabled() bool
}

// inventoryClientOn is the client's own setting: on unless the developer or the installation
// turned it off. It can only switch the inventory off, never on.
func inventoryClientOn(r Reviewer) bool {
	p, ok := r.(inventoryPolicy)
	return ok && p.AgentInventoryEnabled()
}

// accountInventoryOn is the account's switch (`feature_agent_inventory`), which staff turn on
// per customer. The server sends it on POST /policy/check and the plugin remembers it per
// license (policycheck.AgentInventoryOn).
//
// ⚠️ OFF UNTIL THE SERVER SAYS ON. A license the server has not answered yet, a server that
// cannot be reached and a server without the switch all read as off, so nothing of the
// inventory is read or leaves the computer for an account that was not switched on. This is the
// opposite of the plugin's usual fail-open, on purpose: the failure here must be "sent nothing".
//
// A package var so a test can set it.
var accountInventoryOn = func() bool { return policycheck.AgentInventoryOn(licenseKey()) }

// inventoryEnabled needs both: the account switched on, and the client not switched off.
func inventoryEnabled(r Reviewer) bool {
	return inventoryClientOn(r) && accountInventoryOn()
}

// inventoryShipper is the optional half of a Reviewer that sends an installation snapshot. Found
// by asking, like inventoryPolicy: the cloud tier has it and POSTs /inventory.
type inventoryShipper interface {
	ShipInventory(req wire.InventoryRequest) error
}

// inventoryWait is how long the hook waits, counted from when the snapshot was started, for a
// snapshot that is being sent beside another request. Reading the set-up takes well under a
// second and the request gives up after five, so this is only ever reached by something stuck (a
// configuration file on a filesystem that does not answer). The hook then stops waiting and
// exits; the snapshot is lost until the next day's.
var inventoryWait = 10 * time.Second

// startInventory runs shipInventory beside the request the caller is about to make and returns
// the func that waits for it. The caller defers that func, so the hook does not exit while the
// snapshot is still on its way.
//
// This is what keeps the snapshot off the developer's wait: a review takes seconds, so by the
// time it answers the snapshot has long been sent and the wait returns at once. On a turn with
// no review it overlaps the telemetry request instead of adding to it.
//
// ⚠️ A PANIC IN HERE MUST NOT REACH THE RUNTIME. main's backstop covers only its own goroutine,
// and an unrecovered panic in this one would end the hook with a non-zero exit, which an agent
// reads as a failed Stop. It is recovered and logged, and the turn goes on without a snapshot.
func startInventory(meta wire.TurnMeta, a agent.Agent, r Reviewer, ev agent.Event, log *slog.Logger, now time.Time) (wait func()) {
	if _, ok := a.(agent.Inventorier); !ok || !inventoryEnabled(r) {
		return func() {}
	}
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				log.Warn("agent inventory: collecting the snapshot failed (continuing)", "panic", fmt.Sprint(p))
			}
		}()
		shipInventory(meta, a, r, ev, log, now)
	}()
	return func() {
		// Looked at first and by itself: after a long review both this and an expired timer are
		// ready, and a select between them would report a finished snapshot as late.
		select {
		case <-done:
			return
		default:
		}
		left := inventoryWait - time.Since(started)
		if left <= 0 {
			left = 0
		}
		timer := time.NewTimer(left)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			log.Warn("agent inventory: the snapshot was not sent in time; not waiting for it")
		}
	}
}

// SessionEnd handles the end of a session (agent.EventSessionEnd): it sends the installation
// snapshot of a session that ended before it sent one, when this machine reported none in the
// last day (inventory.CollectAtEnd has the reasons). It is what lets a computer that only ever
// runs sessions shorter than the settle wait appear in the inventory at all.
//
// ⚠️ IT IS NOT A STOP. It reviews nothing, reads no changed files, shows no notice and writes
// nothing to stdout. It sends what a turn's snapshot request sends and nothing else, and it
// keeps the organisation's exclusion check: a repository excluded from review sends nothing here
// either.
//
// Every failure is logged and ignored. The agent gives this hook a short time to finish, so it
// asks the cheap question first (is a snapshot wanted at all) and reads the transcript and the
// agent's configuration only when the answer is yes.
func SessionEnd(a agent.Agent, r Reviewer, ev agent.Event) {
	log := slog.With("agent", a.Name())
	now := nowFunc()
	inv, ok := a.(agent.Inventorier)
	// Only the client's setting is asked here. The account's switch is asked after the policy
	// check below, which is what learns it: a session that ends before its first Stop has made
	// no check of its own.
	if !ok || !inventoryClientOn(r) {
		return
	}
	shipper, ok := r.(inventoryShipper)
	if !ok {
		return
	}
	// A session with no transcript the adapter can read sends none, and is not marked: the
	// servers a session loaded and the agent's version are read from the transcript, so the
	// snapshot would list no connectors and look like a set-up that lost them. A session that
	// was opened and never prompted is the usual case (several end at once when an editor
	// closes), and another session of the same machine reports the full picture.
	if ev.TranscriptPath == "" {
		return
	}
	if !inventory.CollectAtEnd(a.Name(), ev.SessionID, now) {
		return
	}
	if pc, ok := r.(policycheck.Checker); ok {
		if res := policycheck.Check(pc, licenseKey(), []string{vcs.RepoOrigin(ev.Cwd)}, now); res.Skip {
			log.Info("agent inventory: not sent at session end: organisation review policy", "repo", res.Repo)
			return
		}
	}
	if !accountInventoryOn() {
		log.Debug("agent inventory: not sent at session end: not switched on for this account")
		return
	}
	// Said here because nothing else marks this hook in the log: the lines that follow read the
	// same as a Stop's.
	log.Info("agent inventory: the session ended without sending a snapshot; sending it now")
	meta := turnMeta(a, r, ev, log, now)
	sendSnapshot(meta, inv, shipper, a.Name(), ev, log, now)
}

// shipInventory sends the agent's installation snapshot on a request of its own when one is due:
// the adapter can describe its install, the inventory is switched on, this session has settled
// and has not collected one yet, and the snapshot differs from every one sent in the last day
// (internal/inventory).
//
// ⚠️ NOT ON THE TURN'S REQUEST. The snapshot is about the installation, so it goes to /inventory
// and never rides /review or /telemetry. meta is read only for the few facts that say which
// installation this is and whose (the request carries no prompt, tokens or model). The per-turn
// half of the inventory is turnMeta's and stays on the turn.
//
// Called through startInventory where a turn's metadata leaves on a turn's FIRST Stop, beside
// the /review request and the /telemetry request, so a session sends one snapshot at most.
//
// Analytics, so it never fails the turn: any error is logged at debug and the turn proceeds.
func shipInventory(meta wire.TurnMeta, a agent.Agent, r Reviewer, ev agent.Event, log *slog.Logger, now time.Time) {
	inv, ok := a.(agent.Inventorier)
	if !ok || !inventoryEnabled(r) {
		return
	}
	// Asked before the session is marked as collected: a reviewer that cannot send must not use
	// up the session's one snapshot.
	shipper, ok := r.(inventoryShipper)
	if !ok {
		return
	}
	// A session with no transcript the adapter can read sends none, and is not marked: the
	// servers a session loaded and the agent's version are read from the transcript, so the
	// snapshot would list no connectors and look like a set-up that lost them. A session that
	// was opened and never prompted is the usual case (several end at once when an editor
	// closes), and another session of the same machine reports the full picture.
	if ev.TranscriptPath == "" {
		return
	}
	// When this turn began, which is the session's start the first time the session is seen.
	// A turn of unknown length counts as starting now, so its session settles from here.
	started := now
	if meta.DurationMs > 0 {
		started = now.Add(-time.Duration(meta.DurationMs) * time.Millisecond)
	}
	if !inventory.CollectNow(a.Name(), ev.SessionID, started, now) {
		return
	}
	sendSnapshot(meta, inv, shipper, a.Name(), ev, log, now)
}

// sendSnapshot reads the snapshot and sends it when its content is due. The caller has already
// decided that this session collects one now.
func sendSnapshot(meta wire.TurnMeta, inv agent.Inventorier, shipper inventoryShipper, agentName string, ev agent.Event, log *slog.Logger, now time.Time) {
	snapshot, err := inv.Inventory(ev)
	if err != nil {
		log.Debug("agent inventory: part of the snapshot could not be read (continuing)", "err", err.Error())
	}
	// Sorted before it is bounded, so an install with more of one kind than the bound keeps the
	// same ones on every read and hashes the same.
	snapshot = inventory.Normalize(wire.ValidInventory(inventory.Normalize(snapshot)))
	if snapshot == nil || !inventory.IsDue(agentName, snapshot, now) {
		return
	}
	log.Info("agent inventory: sending this session's snapshot",
		"connectors", len(snapshot.Connectors), "mcp_servers", len(snapshot.MCPServers),
		"plugins", len(snapshot.Plugins), "hooks", len(snapshot.Hooks),
		"skills", len(snapshot.Skills), "commands", len(snapshot.Commands),
		"subagents", len(snapshot.Subagents), "programs", len(snapshot.Programs))
	req := wire.InventoryRequest{
		InstallID:       meta.InstallID,
		Agent:           meta.Agent,
		AgentVersion:    meta.AgentVersion,
		Repo:            meta.Repo,
		Developer:       meta.Developer,
		DeveloperSource: meta.DeveloperSource,
		OS:              meta.OS,
		Arch:            meta.Arch,
		ClientVersion:   meta.ClientVersion,
		Environment:     meta.Environment,
		EnvironmentRaw:  meta.EnvironmentRaw,
		Inventory:       snapshot,
	}
	if err := shipper.ShipInventory(req); err != nil {
		// Not recorded as sent, so the next session tries again.
		log.Info("agent inventory: the snapshot was not accepted (ignoring; the next session retries)", "err", err.Error())
		return
	}
	inventory.MarkSent(agentName, snapshot, now)
}
