// Package inventory decides WHEN the agent inventory snapshot (wire.AgentInventory, LEO-822) is
// collected and sent. What is in it is the agent adapter's business (agent.Inventorier).
//
// The snapshot describes an installation, which changes rarely, while the hook runs on every
// turn. So:
//
//   - It is COLLECTED at most once per session. Collecting means opening the agent's
//     configuration files, and a session's set-up is fixed when it starts.
//   - It is collected only once the session has SETTLED: settleAfter after it started. An agent
//     attaches its connectors and MCP servers in the background over a session's first seconds,
//     so a snapshot taken at a quick first Stop names none or some of them. One such snapshot
//     listed no connectors for an install that has ten, and as the newest it replaced a complete
//     one. A session that ends sooner sends none, which loses nothing: the next one does.
//   - A session that ENDS before it collected one gets a second chance, at the end of the session
//     (CollectAtEnd), but only when this machine sent no snapshot at all in the last day. A
//     session that short may have ended before the agent wrote its connectors down, so its
//     snapshot can be short of them: it is good enough to say the installation exists, and not
//     good enough to replace a full one from the same day.
//   - It is SENT only when its content differs from every snapshot the server accepted in the
//     last day (IsDue, MarkSent). A
//     machine whose set-up never changes sends one a day, which is also what tells the server
//     the installation is still there.
//
// Both facts live in one small state file under the scratch root, because the hook is a fresh
// process on every event.
//
// ⚠️ EVERY FAILURE HERE MEANS "DO NOT COLLECT, DO NOT SEND". This is the opposite of
// notify.FirstThisSession, which shows a notice it cannot prove was shown. A state file that
// cannot be read or written would otherwise turn "once a day" into "on every turn", for data
// nobody is waiting on. The inventory is analytics: losing one costs a day, never a review.
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/leotrace-hq/leoprevent-plugin/client/internal/scratch"
	"github.com/leotrace-hq/leoprevent-plugin/wire"
)

// resendAfter is how long a sent snapshot keeps an identical one from being sent again.
const resendAfter = 24 * time.Hour

// sessionKept is how long a session's "already collected" mark is kept. Longer than any session
// plausibly runs; a session that outlives it collects once more.
const sessionKept = 7 * 24 * time.Hour

// settleAfter is how long after a session started its snapshot may be collected (see the package
// note).
//
// Measured, not guessed (2026-11-12, Claude Code 2.1.322 in a terminal, four sessions, 27 hosted
// connectors and one local server): nothing was attached when the first prompt was sent, and
// everything was by 3 to 4 seconds in. Hosted connectors attach side by side, so more of them
// does not take proportionally longer. What does is a slow network, and a local server whose
// launcher has to download its package first (`npx`, `uvx`, `docker run` on a cold cache).
//
// 30 seconds is about eight times the measurement, for those. The price of waiting longer is
// small, since only a session that ends sooner sends no snapshot, and the price of waiting too
// little is a snapshot that is short of servers. It also means the Stop that collects comes after
// a later message in the session, which is when the agent writes the servers it has attached to
// the transcript the snapshot reads them from. One machine and one network: raise it if
// snapshots turn up short.
var settleAfter = 30 * time.Second

// SetSettleAfterForTest overrides settleAfter for a test and returns a func restoring it.
func SetSettleAfterForTest(d time.Duration) func() {
	old := settleAfter
	settleAfter = d
	return func() { settleAfter = old }
}

type state struct {
	// Started maps "<agent>/<session id>" to when the session was first seen to have started
	// (Unix seconds): the start of the turn whose Stop first reported it.
	Started map[string]int64 `json:"started,omitempty"`
	// Sessions maps "<agent>/<session id>" to when its snapshot was collected (Unix seconds).
	Sessions map[string]int64 `json:"sessions,omitempty"`
	// Sent maps "<agent>/<snapshot hash>" to when that snapshot was last sent (Unix seconds).
	Sent map[string]int64 `json:"sent,omitempty"`
}

func statePath() string { return filepath.Join(scratch.Dir("inventory"), "state.json") }

// load reads the state. A missing file is an empty state; anything else unreadable is an error,
// which callers treat as "do nothing".
func load() (state, error) {
	st := state{Started: map[string]int64{}, Sessions: map[string]int64{}, Sent: map[string]int64{}}
	data, err := os.ReadFile(statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	if st.Started == nil {
		st.Started = map[string]int64{}
	}
	if st.Sessions == nil {
		st.Sessions = map[string]int64{}
	}
	if st.Sent == nil {
		st.Sent = map[string]int64{}
	}
	return st, nil
}

// save writes the state through a temp file, so a concurrent hook never reads half of one.
func save(st state, now time.Time) error {
	for k, at := range st.Started {
		if now.Sub(time.Unix(at, 0)) > sessionKept {
			delete(st.Started, k)
		}
	}
	for k, at := range st.Sessions {
		if now.Sub(time.Unix(at, 0)) > sessionKept {
			delete(st.Sessions, k)
		}
	}
	for k, at := range st.Sent {
		if now.Sub(time.Unix(at, 0)) > resendAfter {
			delete(st.Sent, k)
		}
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	path := statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "state-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// CollectNow reports whether sessionID's snapshot should be collected on this Stop, and records
// that it now has been: the session has not collected one yet, and it started at least
// settleAfter ago.
//
// started is when the turn ending now began. It is used only the first time the session is seen,
// as the session's start, and kept: a later, shorter turn must not make an old session look
// young. A zero or future value counts as now, so a session whose turn start is unknown collects
// on its first Stop settleAfter later.
//
// False for an empty session id and on any state error (see the package note).
func CollectNow(agentName, sessionID string, started, now time.Time) bool {
	if sessionID == "" {
		return false
	}
	st, err := load()
	if err != nil {
		return false
	}
	key := agentName + "/" + sessionID
	// A mark older than a day no longer counts: a session kept open or resumed for days
	// collects once a day, like a machine that starts a new session every day.
	if at, collected := st.Sessions[key]; collected && now.Sub(time.Unix(at, 0)) < resendAfter {
		return false
	}
	began, known := st.Started[key]
	if !known {
		if started.IsZero() || started.After(now) {
			started = now
		}
		began = started.Unix()
		st.Started[key] = began
	}
	if now.Sub(time.Unix(began, 0)) < settleAfter {
		// Not settled. The start still has to be kept, or the next Stop would measure from its
		// own turn; if it cannot be, that Stop simply measures again.
		if !known {
			_ = save(st, now)
		}
		return false
	}
	st.Sessions[key] = now.Unix()
	return save(st, now) == nil
}

// CollectAtEnd reports whether sessionID's snapshot should be collected as the session ends, and
// records that it now has been: the session has not collected one, and agentName sent no
// snapshot at all in the last resendAfter.
//
// ⚠️ THE SECOND CONDITION IS THE POINT. This runs for a session too short to have settled, and
// such a session can end before the agent has written its connectors to the transcript. Its
// snapshot may therefore list fewer than the installation has. Sent on a machine that already
// reported today, it would be the newest and would hide the full one. Sent on a machine that
// reported nothing today, it is the difference between a partial picture and none, and the next
// session that does settle corrects it.
//
// It does not look at the session's age: a session that is ending will not settle later.
//
// False for an empty session id and on any state error (see the package note).
func CollectAtEnd(agentName, sessionID string, now time.Time) bool {
	if sessionID == "" {
		return false
	}
	st, err := load()
	if err != nil {
		return false
	}
	key := agentName + "/" + sessionID
	if _, collected := st.Sessions[key]; collected {
		return false
	}
	for sent, at := range st.Sent {
		if strings.HasPrefix(sent, agentName+"/") && now.Sub(time.Unix(at, 0)) < resendAfter {
			return false
		}
	}
	st.Sessions[key] = now.Unix()
	return save(st, now) == nil
}

// IsDue reports whether inv should be sent now: its content was not sent in the last day. It
// records nothing; MarkSent does, once the server has accepted the snapshot. False on any state
// error.
func IsDue(agentName string, inv *wire.AgentInventory, now time.Time) bool {
	if inv == nil {
		return false
	}
	st, err := load()
	if err != nil {
		return false
	}
	at, sent := st.Sent[agentName+"/"+Hash(inv)]
	return !sent || now.Sub(time.Unix(at, 0)) >= resendAfter
}

// MarkSent records that inv was sent and accepted, which keeps an identical one from being sent
// again for a day.
//
// Recorded AFTER the request, not before: a request the server refused, one that timed out, or a
// hook that was killed while sending (the end-of-session hook has a second and a half) leaves no
// mark, so the next session sends the snapshot instead of believing it was delivered.
func MarkSent(agentName string, inv *wire.AgentInventory, now time.Time) {
	if inv == nil {
		return
	}
	st, err := load()
	if err != nil {
		return
	}
	st.Sent[agentName+"/"+Hash(inv)] = now.Unix()
	_ = save(st, now)
}

// Normalize sorts every list in inv, so two snapshots of the same installation are equal
// whatever order the configuration files were read in. It returns inv.
func Normalize(inv *wire.AgentInventory) *wire.AgentInventory {
	if inv == nil {
		return nil
	}
	sort.SliceStable(inv.Connectors, func(i, j int) bool {
		a, b := inv.Connectors[i], inv.Connectors[j]
		return a.Name+"\x00"+a.ID < b.Name+"\x00"+b.ID
	})
	sort.SliceStable(inv.MCPServers, func(i, j int) bool {
		a, b := inv.MCPServers[i], inv.MCPServers[j]
		return a.Name+"\x00"+a.Scope < b.Name+"\x00"+b.Scope
	})
	sort.SliceStable(inv.Plugins, func(i, j int) bool {
		a, b := inv.Plugins[i], inv.Plugins[j]
		return a.Name+"\x00"+a.Marketplace+"\x00"+a.Scope < b.Name+"\x00"+b.Marketplace+"\x00"+b.Scope
	})
	sort.SliceStable(inv.Hooks, func(i, j int) bool {
		a, b := inv.Hooks[i], inv.Hooks[j]
		return a.Source+"\x00"+a.Plugin+"\x00"+a.Event+"\x00"+a.Hash < b.Source+"\x00"+b.Plugin+"\x00"+b.Event+"\x00"+b.Hash
	})
	for _, list := range [][]wire.InventoryInstruction{inv.Skills, inv.Commands, inv.Subagents} {
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			return a.Source+"\x00"+a.Plugin+"\x00"+a.Name < b.Source+"\x00"+b.Plugin+"\x00"+b.Name
		})
	}
	sort.SliceStable(inv.Programs, func(i, j int) bool {
		a, b := inv.Programs[i], inv.Programs[j]
		return a.Kind+"\x00"+a.Source+"\x00"+a.Hash < b.Kind+"\x00"+b.Source+"\x00"+b.Hash
	})
	return inv
}

// Hash is a short content hash of a normalized snapshot. It never leaves the machine: it only
// keys the "already sent" state.
func Hash(inv *wire.AgentInventory) string {
	data, _ := json.Marshal(inv)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
