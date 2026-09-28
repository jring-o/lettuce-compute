package daemon

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// Skipping units this machine cannot finish before their deadline.
//
// A unit must be finished within its deadline, counted from the moment it
// starts. A machine too slow for a leaf ran its units to the deadline and
// had them stopped there — every hour of it wasted, and the unit late for the
// volunteer who got it next. The head refuses such a machine only when the
// leaf publishes an FP-ops estimate to judge it by, which no leaf does, so the
// client — which knows how long the leaf's units take here — judges instead:
//
//   - The leaf's run time here is the median of its recent completions
//     (DurationTracker), scaled to the most cores a task of it could now be
//     given: its declared maximum, or the volunteer's cores setting for it,
//     within the CPU limit. A leaf that uses the cores it declares runs
//     proportionally faster on more of them, so raising them is the remedy a
//     notice can offer.
//   - A unit needs that time plus a margin (deadlineMargin), stretched by the
//     CPU time limit (a unit running half the time takes twice as long).
//   - It has its deadline, less the hours the schedule keeps work stopped.
//
// A leaf whose units need more than they have is not asked for, a unit of it
// that arrives anyway (the first batch, before its deadline is known) is
// given back un-run, and one already buffered is given back un-run by the
// buffer sweep — budget-neutral, so another volunteer finishes it in time.
// A notice names the leaf, the figures and what would help. Everything is
// worked out afresh each time: a raised core limit, a cores setting for the
// leaf or a faster run clears it on its own. Before the leaf's first
// completion here there is no run time to judge by, and it is fetched as
// before.

// deadlineMargin is how much longer than its learned run time a unit is
// allowed to need: 25 %, so a leaf whose units run close to their deadline is
// not fetched only to be stopped at it.
const deadlineMargin = 1.25

// deadlineCheck is how a leaf's units measure against their deadline on this
// machine.
type deadlineCheck struct {
	// Infeasible is true when a unit cannot finish in time here.
	Infeasible bool
	// Cores is the most cores a task of the leaf could be given here, and
	// RunSeconds the leaf's learned run time at that many, from Runs
	// completions (0 when there are none yet: nothing to judge by).
	Cores      int
	RunSeconds float64
	Runs       int
	// NeedSeconds is the wall-clock time a unit needs: RunSeconds with the
	// margin, stretched by the CPU time limit (CPUTimePct).
	NeedSeconds float64
	CPUTimePct  int
	// Deadline is the unit's deadline in seconds, and OpenSeconds how much of
	// it the schedule lets work run (ScheduleKnown false when that cannot be
	// told in advance: "run when idle").
	Deadline      int32
	OpenSeconds   float64
	ScheduleKnown bool
}

// checkDeadline judges a leaf's units against a deadline of deadline seconds
// counted from now.
func (d *Daemon) checkDeadline(leaf CachedLeafInfo, deadline int32) deadlineCheck {
	c := deadlineCheck{Deadline: deadline, CPUTimePct: d.cpuTimePct(), ScheduleKnown: true}
	if deadline <= 0 || d.durations == nil || leaf.ID == "" {
		return c
	}
	_, c.Cores = d.unitCoreRange(leafShapeUnit(leaf))
	run, ok := d.durations.SecondsAt(leaf.ID, c.Cores)
	if !ok {
		return c
	}
	c.RunSeconds, c.Runs = run, d.durations.Completions(leaf.ID)
	c.NeedSeconds = run * deadlineMargin / d.cpuTimeFraction()
	c.OpenSeconds = float64(deadline)
	if d.scheduler != nil {
		c.OpenSeconds, c.ScheduleKnown = d.scheduler.RunSecondsWithin(time.Now(), time.Duration(deadline)*time.Second)
	}
	c.Infeasible = c.NeedSeconds > c.OpenSeconds
	return c
}

// leafDeadline is the deadline, in seconds, the leaf's units carry: that of
// the last one to arrive, else of the last one to complete here; 0 when
// neither is known.
func (d *Daemon) leafDeadline(leafID string) int32 {
	d.arrivalEstMu.Lock()
	deadline := d.arrivalDeadline[leafID]
	d.arrivalEstMu.Unlock()
	if deadline > 0 {
		return deadline
	}
	if d.durations != nil {
		if deadline, ok := d.durations.Deadline(leafID); ok {
			return deadline
		}
	}
	return 0
}

// noteArrivalDeadline records the deadline of a just-arrived unit of the
// leaf, so the leaf is judged by it before its next request.
func (d *Daemon) noteArrivalDeadline(leafID string, deadline int32) {
	if leafID == "" || deadline <= 0 {
		return
	}
	d.arrivalEstMu.Lock()
	if d.arrivalDeadline == nil {
		d.arrivalDeadline = make(map[string]int32)
	}
	d.arrivalDeadline[leafID] = deadline
	d.arrivalEstMu.Unlock()
}

// leafDeadlineGate is the fetcher's pre-request skip: false, with the reason,
// for a leaf whose units cannot finish before their deadline here.
func (d *Daemon) leafDeadlineGate(leaf CachedLeafInfo) (bool, string) {
	c := d.checkDeadline(leaf, d.leafDeadline(leaf.ID))
	if !c.Infeasible {
		return true, ""
	}
	return false, "cannot finish before its deadline on this machine: " + c.figures()
}

// LeafDeadlineStatus reports whether the fetcher is skipping a leaf because
// its units cannot finish before their deadline on this machine, and why.
func (d *Daemon) LeafDeadlineStatus(leaf CachedLeafInfo) (blocked bool, reason string) {
	ok, why := d.leafDeadlineGate(leaf)
	return !ok, why
}

// unitDeadlineUnfit is the reason a unit cannot finish before its own
// deadline here, or "": the check a unit that arrives, or waits in the
// buffer, is given back un-run by.
func (d *Daemon) unitDeadlineUnfit(wu *runtime.WorkUnit) string {
	if wu == nil {
		return ""
	}
	c := d.checkDeadline(d.leafOfUnit(wu), wu.DeadlineSeconds)
	if !c.Infeasible {
		return ""
	}
	return "cannot finish before its deadline on this machine: " + c.figures()
}

// figures is the check's arithmetic in one line, for logs and give-back
// reasons: "needs about 6 h 15 min (a median 5 h at 2 cores, +25 %), has 6
// hours".
func (c deadlineCheck) figures() string {
	need := fmt.Sprintf("needs about %s (a median %s at %s, +%d %%",
		roughDuration(int64(c.NeedSeconds)), roughDuration(int64(c.RunSeconds)), plural(c.Cores, "core"), int(math.Round((deadlineMargin-1)*100)))
	if c.CPUTimePct < 100 {
		need += fmt.Sprintf(", at %d %% CPU time", c.CPUTimePct)
	}
	need += ")"
	has := fmt.Sprintf("has %s", roughDuration(int64(c.Deadline)))
	if c.OpenSeconds < float64(c.Deadline) {
		has += fmt.Sprintf(", of which the schedule leaves %s to run", roughDuration(int64(c.OpenSeconds)))
	}
	return need + ", " + has
}

// Notice code for a leaf this machine is too slow for (per head and leaf).
// Distinct from the head's own infeasible_deadline reason, which the no-work
// machinery keeps.
const noticeDeadlineSkip = "deadline_too_short_here"

// deadlineNoticeMessage words the notice for a leaf this machine cannot
// finish in time, in the shape of the head's own infeasible-deadline reason,
// with what would help: more cores for the leaf where it can use them, a
// higher CPU time share, a wider schedule, or disabling it here.
func (d *Daemon) deadlineNoticeMessage(headName string, leaf CachedLeafInfo, c deadlineCheck) string {
	name := leafNoticeLabel(leaf)
	if name == "" {
		name = leaf.ID
	}
	runs := "its last run here"
	if c.Runs > 1 {
		runs = fmt.Sprintf("the middle of its last %d runs here", c.Runs)
	}
	msg := fmt.Sprintf("%s's tasks on %s must finish within %s. On this machine one takes about %s at %s (%s)",
		name, headName, roughDuration(int64(c.Deadline)), roughDuration(int64(c.RunSeconds)), plural(c.Cores, "core"), runs)
	if c.CPUTimePct < 100 {
		msg += fmt.Sprintf(", and at your %d %% CPU time limit it runs only that share of the time", c.CPUTimePct)
	}
	if c.OpenSeconds < float64(c.Deadline) {
		msg += fmt.Sprintf(", and your schedule leaves only %s of the deadline to run in", roughDuration(int64(c.OpenSeconds)))
	}
	msg += fmt.Sprintf(" — about %s with a margin, so it would not finish in time. Lettuce does not fetch %s here and gives back any it holds, un-run, so another volunteer can finish them.",
		roughDuration(int64(c.NeedSeconds)), name)

	var help []string
	_, declaredMax := d.declaredCoreRange(leaf.ID)
	if budget := d.cpuBudgetFor(leafShapeUnit(leaf)); c.Cores < declaredMax {
		if override := d.leafOverride(leaf.ID).cores; override > 0 && override < declaredMax && override < budget {
			help = append(help, fmt.Sprintf("give its tasks more cores — it can use up to %d, and you set %d (Projects → %s → Cores per task, or `lettuce-volunteer leafs cores %s %d`)",
				declaredMax, override, name, leafSlugOrID(leaf), declaredMax))
		} else {
			help = append(help, fmt.Sprintf("raise your CPU limit — its tasks can use up to %d cores, and your limit gives them at most %d", declaredMax, c.Cores))
		}
	}
	if c.CPUTimePct < 100 {
		help = append(help, "raise the CPU time limit")
	}
	if c.OpenSeconds < float64(c.Deadline) {
		help = append(help, "widen your schedule")
	}
	help = append(help, fmt.Sprintf("disable %s on this machine", name))
	return msg + " To run it here: " + strings.Join(help, "; or ") + "."
}

// leafSlugOrID is how a command names a leaf: its slug, else its id.
func leafSlugOrID(leaf CachedLeafInfo) string {
	if leaf.Slug != "" {
		return leaf.Slug
	}
	return leaf.ID
}

// refreshDeadlineNotices keeps one notice per enabled leaf this machine
// cannot finish in time, worked out afresh, and resolves the notice of any
// leaf that can again (or is no longer enabled).
func (d *Daemon) refreshDeadlineNotices() {
	live := make(map[headLeafKey]bool)
	for _, hl := range d.enabledLeafsByHead() {
		c := d.checkDeadline(hl.leaf, d.leafDeadline(hl.leaf.ID))
		key := headLeafKey{head: hl.head, leaf: hl.leaf.ID}
		if !c.Infeasible {
			continue
		}
		live[key] = true
		msg := d.deadlineNoticeMessage(hl.head, hl.leaf, c)
		if d.setupNotices.changed(noticeDeadlineSkip, key, msg) {
			d.notices.Notify(NoticeWarn, noticeDeadlineSkip, msg, key.head, key.leaf)
			d.logger.Warn("not fetching a leaf this machine cannot finish before its deadline",
				"head", key.head, "leaf_id", key.leaf, "leaf_name", leafNoticeLabel(hl.leaf),
				"run_seconds", int(c.RunSeconds), "cores", c.Cores, "need_seconds", int(c.NeedSeconds),
				"deadline_seconds", c.Deadline, "open_seconds", int(c.OpenSeconds))
		}
	}
	for _, key := range d.setupNotices.forgetExcept(noticeDeadlineSkip, live) {
		d.notices.Resolve(noticeDeadlineSkip, key.head, key.leaf)
		d.logger.Info("a leaf can finish before its deadline here again; fetching it", "head", key.head, "leaf_id", key.leaf)
	}
}

// setupNoticeState remembers, per notice code and head/leaf, the message a
// re-evaluated notice last carried, so a condition checked every half minute
// is announced when it begins or changes — not counted up on every check —
// and resolved when it ends.
type setupNoticeState struct {
	last map[string]map[headLeafKey]string
}

// changed records msg for code and key and reports whether it differs from
// the last one recorded.
func (s *setupNoticeState) changed(code string, key headLeafKey, msg string) bool {
	if s.last == nil {
		s.last = make(map[string]map[headLeafKey]string)
	}
	m := s.last[code]
	if m == nil {
		m = make(map[headLeafKey]string)
		s.last[code] = m
	}
	if prev, ok := m[key]; ok && prev == msg {
		return false
	}
	m[key] = msg
	return true
}

// forgetExcept drops every key of code not in live and returns them: the
// conditions that ended.
func (s *setupNoticeState) forgetExcept(code string, live map[headLeafKey]bool) []headLeafKey {
	var ended []headLeafKey
	for key := range s.last[code] {
		if !live[key] {
			ended = append(ended, key)
			delete(s.last[code], key)
		}
	}
	return ended
}
