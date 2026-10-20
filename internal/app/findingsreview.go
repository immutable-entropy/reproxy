package app

import (
	"log/slog"
	"sync"

	"reproxy/internal/agent"
	"reproxy/internal/config"
	"reproxy/internal/findings"
)

// The findings reviewer: a background agent session that watches the findings
// store and scores each new finding's confidence, so a human doesn't have to
// triage a large volume of noise by hand (see internal/findings for the
// score). It is a core service owned by App, like the crawler and the
// recorder, driven through the same agent runtime and MCP tools the in-app
// chat uses.
//
// It is enabled per agent profile (AgentProfile.ReviewFindings). The
// designated reviewer is the first profile whose toggle is on; it drives one
// reused session so restarts don't pile them up, in auto mode so a background
// turn never blocks on an approval no one is watching for.
//
// The brief is said once, as the session's standing instructions (sent with
// the system prompt), and each round of new findings is a one-line nudge.
// Each round also starts on a cleared context: the work left is whatever the
// store has unscored, so nothing from an earlier round is needed, and
// carrying it would send every past round's tool results with every request
// of every later one.

// reviewTitle names the session the reviewer drives, so it is recognised and
// reused rather than a new one made each launch.
const reviewTitle = "Findings review"

// reviewInstructions are the review session's standing instructions
// (agent.Session.Instructions).
const reviewInstructions = "You are an expert security and quality assurance engineer that has been tasked with reviewing new " +
	"findings discovered as part of a Security Assessment on a number of scoped systems. Findings are recorded " +
	"by a number of automations and you must determine (a) if they warrant further review, and (b) two numeric scores: " +
	"a confidence and a concern. Review every finding that has not been scored yet: list them with list_findings " +
	"(unreviewed: true), and for each read its value, its sightings and the surrounding traffic. First judge whether it " +
	"is a true or a false positive given its context and location, and call set_finding_confidence with a score from 0 " +
	"to 100 — near 0 for certain noise, near 100 for a certain true positive, and 50 only when there genuinely isn't " +
	"enough to tell. Then judge how big an issue it would be from a security or QA standpoint, and call " +
	"set_finding_concern with a score from 0 to 100 — 0 for no concern, 100 for a critical issue likely to lead to " +
	"exploitation. Score both for every finding. Only read and score; do not change or send any traffic. When every " +
	"new finding has a confidence and a concern, stop."

// reviewNudge starts each round of review.
const reviewNudge = "New findings are waiting to be reviewed."

// findingsReviewer drives the review session off the findings store's events.
type findingsReviewer struct {
	mgr *agent.Manager
	log *slog.Logger

	wake chan struct{}
	quit chan struct{}
	done chan struct{}

	mu        sync.Mutex
	enabled   bool
	profileID string
	sessionID string
	dirty     bool // new findings are waiting to be reviewed
}

// newFindingsReviewer starts the reviewer's loop. It does nothing until
// configure names a reviewing profile.
func newFindingsReviewer(mgr *agent.Manager, log *slog.Logger) *findingsReviewer {
	r := &findingsReviewer{
		mgr:  mgr,
		log:  log,
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go r.loop()
	return r
}

// configure sets the designated reviewing profile from the agent config: the
// first profile whose toggle is on, or none. Called at startup and whenever
// the agent configuration is saved.
func (r *findingsReviewer) configure(ag config.Agents) {
	var profile string
	for _, p := range ag.Profiles {
		if p.ReviewsFindings() {
			profile = p.ID
			break
		}
	}
	r.mu.Lock()
	if profile != r.profileID {
		r.sessionID = "" // a different agent reviews now; it gets its own session
	}
	r.profileID = profile
	r.enabled = profile != ""
	r.mu.Unlock()

	// Reconcile the review sessions with the config. Keep (and, below, adopt)
	// the one for the enabled profile; remove any other — a stale session from
	// a reviewer that was turned off, or from a previous agent. The user can't
	// close a managed session, so turning the setting off is what removes it.
	for _, s := range r.mgr.List() {
		if s.Title == reviewTitle && !(r.enabled && s.ProfileID == profile) {
			r.mgr.Close(s.ID)
		}
	}
	// Show the session as soon as reviewing is enabled, before any finding has
	// arrived, so it is visible that the reviewer is on.
	if r.enabled {
		r.ensureSession(profile)
	}
	r.signal()
}

// onFinding is subscribed to the findings store. It runs on the goroutine that
// recorded the finding and must not block, so it only flags work and wakes the
// loop.
func (r *findingsReviewer) onFinding(e findings.Event) {
	switch e.Kind {
	case findings.Added:
		if e.Finding.Confidence != nil {
			return // already scored as it was raised
		}
		r.mu.Lock()
		self := r.sessionID
		r.mu.Unlock()
		if self != "" && e.Finding.Author == "agent session "+self {
			return // don't review what the reviewer's own agent recorded
		}
		r.mark()
	case findings.Reset:
		// The engagement changed: any queued work was for the old one.
		r.mu.Lock()
		r.dirty = false
		r.mu.Unlock()
	}
}

// onAgent is subscribed to the agent manager, to wake the loop when the review
// session finishes a turn and can take the next batch.
func (r *findingsReviewer) onAgent(e agent.Event) {
	if e.Kind != agent.SessionChanged {
		return
	}
	r.mu.Lock()
	sid := r.sessionID
	r.mu.Unlock()
	if e.Summary.ID == sid && e.Summary.Status != agent.StatusRunning {
		r.signal()
	}
}

func (r *findingsReviewer) mark() {
	r.mu.Lock()
	r.dirty = true
	enabled := r.enabled
	r.mu.Unlock()
	if enabled {
		r.signal()
	}
}

func (r *findingsReviewer) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *findingsReviewer) loop() {
	defer close(r.done)
	for {
		select {
		case <-r.quit:
			return
		case <-r.wake:
			r.step()
		}
	}
}

func (r *findingsReviewer) step() {
	r.mu.Lock()
	if !r.enabled || r.profileID == "" || !r.dirty {
		r.mu.Unlock()
		return
	}
	sid, profile := r.sessionID, r.profileID
	r.mu.Unlock()

	if sid == "" {
		sid = r.ensureSession(profile)
		if sid == "" {
			return
		}
	}
	s, ok := r.mgr.Get(sid)
	if !ok {
		// The user closed the review session: forget it and make a new one.
		r.mu.Lock()
		r.sessionID = ""
		r.mu.Unlock()
		r.signal()
		return
	}
	if s.Status == agent.StatusRunning {
		return // busy; onAgent wakes us when the turn finishes
	}

	// Take the work and start a turn on a fresh context. Clear the flag
	// first; if the turn can't be started, put it back so the next wake
	// retries.
	r.mu.Lock()
	r.dirty = false
	r.mu.Unlock()
	err := r.mgr.ClearContext(sid)
	if err == nil {
		err = r.mgr.Send(sid, reviewNudge)
	}
	if err != nil {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
		if r.log != nil {
			r.log.Debug("findings reviewer couldn't start a turn", "err", err)
		}
	}
}

// ensureSession reuses the profile's review session if one is still around (so
// restarts don't pile them up), otherwise creates one in auto mode. Either way
// it is marked managed, so the user sees it but can't close it from under the
// reviewer (it is turned off through the agent's config instead).
func (r *findingsReviewer) ensureSession(profile string) string {
	for _, s := range r.mgr.List() {
		if s.ProfileID == profile && s.Title == reviewTitle {
			_ = r.mgr.SetManaged(s.ID, true)
			// A session made before the brief was an instruction gets it now.
			_ = r.mgr.SetInstructions(s.ID, reviewInstructions)
			r.mu.Lock()
			r.sessionID = s.ID
			r.mu.Unlock()
			return s.ID
		}
	}
	sum := r.mgr.Create(profile, reviewTitle)
	// Auto mode so the background turn never blocks waiting for an approval
	// no one is watching for; the instruction keeps it to reading and scoring.
	_ = r.mgr.SetMode(sum.ID, agent.ModeAuto)
	_ = r.mgr.SetManaged(sum.ID, true)
	_ = r.mgr.SetInstructions(sum.ID, reviewInstructions)
	r.mu.Lock()
	r.sessionID = sum.ID
	r.mu.Unlock()
	return sum.ID
}

// stop ends the loop; any pending work is dropped.
func (r *findingsReviewer) stop() {
	select {
	case <-r.quit:
	default:
		close(r.quit)
	}
	<-r.done
}
