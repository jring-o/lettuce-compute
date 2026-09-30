package daemon

import (
	"fmt"
	"sort"
	"strings"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// Why a head sent no work.
//
// A head's empty work reply used to carry no reason, so every empty answer fed one
// generic diagnostic: "the attached heads have no units matching this machine", with
// advice about runtimes, disk space and the schedule. That advice was wrong whenever the
// head was holding this machine at its per-machine in-flight cap, or this account already
// had a result on every unit, or a recent failed copy had benched it from a unit, or the
// account itself was benched, or the head judged it too slow for a unit's deadline. A
// current head names those causes on the reply (RequestWorkUnitResponse.no_work_reason),
// and the fetcher now:
//
//   - raises that reason's own notice, keyed to the head (and the leaf, where the reason
//     is about a leaf's units), instead of feeding the generic "no units matching" streak;
//     the two reasons that ask nothing of the volunteer (this account has done every task
//     ready, a recent failed copy of its own) are informational, and are kept as one line
//     per head naming every leaf they cover rather than a card per leaf;
//   - on INFLIGHT_CAP, stops asking that head until this machine holds fewer of its units
//     than it did when the head answered — a copy finished or was given back — because
//     asking again sooner only repeats the answer;
//   - keeps the head's last reason for `status` and `doctor`.
//
// A head that predates the field answers UNSPECIFIED, which behaves exactly as before.

// capWaitMax bounds how long a head that answered INFLIGHT_CAP is left alone when none of
// this machine's copies of its units finishes: the head's cap can also rise without a
// completion here (its reliability quota ramps as other volunteers validate this
// machine's results), and a stale count must never park a head for good.
const capWaitMax = 10 * time.Minute

// Notice codes for the reasons a head names. The head-level ones concern the machine or
// the account on that head; the leaf-level ones concern one leaf's units there.
const (
	noticeInflightCap        = "inflight_cap"
	noticeAccountBenched     = "account_benched"
	noticeAlreadyContributed = "already_contributed"
	noticeBenchCooldown      = "bench_cooldown"
	noticeInfeasibleDeadline = "infeasible_deadline"
)

var (
	headReasonNotices = []string{noticeInflightCap, noticeAccountBenched}
	// waitingReasonNotices are the informational reasons: one Info notice per head,
	// worded for every leaf the head answers with that reason.
	waitingReasonNotices = []string{noticeAlreadyContributed, noticeBenchCooldown}
)

// noWorkNoticeCode maps a reason to its notice code ("" for UNSPECIFIED or a value this
// build does not know, which a newer head may send and which is then treated as no
// reason at all).
func noWorkNoticeCode(r lettucev1.NoWorkReason) string {
	switch r {
	case lettucev1.NoWorkReason_NO_WORK_REASON_INFLIGHT_CAP:
		return noticeInflightCap
	case lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED:
		return noticeAccountBenched
	case lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED:
		return noticeAlreadyContributed
	case lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN:
		return noticeBenchCooldown
	case lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE:
		return noticeInfeasibleDeadline
	}
	return ""
}

// noWorkReasonMessage words a head's reason for the volunteer. leafName is "" for a
// request that named no leaf. idle and slots are this machine's starved idle slots and
// its slot count; the cap's wording mentions the idle slots only when there are some.
func noWorkReasonMessage(headName, leafName string, resp *lettucev1.RequestWorkUnitResponse, idle, slots int) string {
	switch resp.GetNoWorkReason() {
	case lettucev1.NoWorkReason_NO_WORK_REASON_INFLIGHT_CAP:
		held := fmt.Sprintf("%s lets this machine hold %s at a time right now, and this machine holds %d",
			headName, plural(int(resp.GetInflightCap()), "task"), resp.GetInflightHeld())
		if idle > 0 {
			held += fmt.Sprintf(", so %d of its %s %s idle", idle, plural(slots, "slot"), isAre(idle))
		}
		return held + ". More will come as these finish. This is the head's limit, not a setting on your computer."
	case lettucev1.NoWorkReason_NO_WORK_REASON_ALREADY_CONTRIBUTED:
		return waitingSentence(noticeAlreadyContributed, []waitingLeaf{{label: leafName, head: headName}})
	case lettucev1.NoWorkReason_NO_WORK_REASON_BENCH_COOLDOWN:
		return waitingSentence(noticeBenchCooldown, []waitingLeaf{{label: leafName, head: headName}})
	case lettucev1.NoWorkReason_NO_WORK_REASON_ACCOUNT_BENCHED:
		if until := resp.GetBenchedUntilUnix(); until > 0 {
			return fmt.Sprintf("%s has paused sending work to this account until %s. It resumes on its own.",
				headName, time.Unix(until, 0).UTC().Format("2006-01-02 15:04 UTC"))
		}
		return fmt.Sprintf("%s has paused sending work to this account. No end time is set: it lasts until the head's operator lifts it.", headName)
	case lettucev1.NoWorkReason_NO_WORK_REASON_INFEASIBLE_DEADLINE:
		tasks := "Its tasks"
		if leafName != "" {
			tasks = leafName + "'s tasks"
		}
		within := fmt.Sprintf("%s on %s must finish within %s", tasks, headName, roughDuration(resp.GetDeadlineSeconds()))
		if est := resp.GetEstimatedSeconds(); est > 0 {
			return fmt.Sprintf("%s. At the speed %s has on record for your account, one would take about %s, so the head gives them to faster machines. Nothing to change on your side.",
				within, headName, roughDuration(est))
		}
		return fmt.Sprintf("%s, and at the speed %s has on record for your account one would not, so the head gives them to faster machines. Nothing to change on your side.",
			within, headName)
	}
	return ""
}

// waitingSentence words an informational reason for the leaves of one head that
// gave it; "" for none. It names no command, because the desktop app shows it
// too: `status` and `doctor` add their own pointer to `leafs list`.
func waitingSentence(code string, leaves []waitingLeaf) string {
	if len(leaves) == 0 {
		return ""
	}
	head := leaves[0].head
	var names []string
	anyLeaf := false
	for _, wl := range leaves {
		if wl.label == "" {
			anyLeaf = true
			continue
		}
		names = append(names, wl.label)
	}
	sort.Strings(names)
	switch code {
	case noticeAlreadyContributed:
		scope := "for " + leafList(names)
		if anyLeaf || len(names) == 0 {
			scope = "for this machine"
		}
		return fmt.Sprintf("This account already has a result on, or holds a copy of, every task %s has ready %s. "+
			"Each task needs results from different volunteers, so these are waiting for others. New tasks will reach you.", head, scope)
	case noticeBenchCooldown:
		if len(names) > 1 {
			return fmt.Sprintf("Recent copies of %s tasks on %s, run by this account, did not finish, so the head is offering those tasks to other volunteers first, for about one task deadline. "+
				"Nothing to change unless it keeps happening.", leafList(names), head)
		}
		task := "a task"
		if len(names) == 1 {
			task = "a " + names[0] + " task"
		}
		return fmt.Sprintf("A recent copy of %s on %s, run by this account, did not finish, so the head is offering that task to other volunteers first, for about one task deadline. "+
			"Nothing to change unless it keeps happening.", task, head)
	}
	return ""
}

// leafList names leaves compactly: "A", "A and B", "A, B and C", and past three
// the first two and a count ("A, B and 4 more").
func leafList(names []string) string {
	switch n := len(names); {
	case n == 0:
		return ""
	case n == 1:
		return names[0]
	case n <= 3:
		return strings.Join(names[:n-1], ", ") + " and " + names[n-1]
	default:
		return fmt.Sprintf("%s, %s and %d more", names[0], names[1], n-2)
	}
}

// plural renders "1 task" / "3 tasks".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// roughDuration renders whole seconds as a short human figure (45 seconds, 12 minutes,
// 3 h 5 min).
func roughDuration(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 120:
		return plural(int(secs), "second")
	case secs < 2*3600:
		return plural(int((secs+30)/60), "minute")
	default:
		m := (secs + 30) / 60
		if m%60 == 0 {
			return plural(int(m/60), "hour")
		}
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
}

// leafNoticeLabel is how a leaf is named in a reason's wording: its name, else its slug,
// else its id; "" for the any-leaf request.
func leafNoticeLabel(leaf CachedLeafInfo) string {
	switch {
	case leaf.ID == "":
		return ""
	case leaf.Name != "":
		return leaf.Name
	case leaf.Slug != "":
		return leaf.Slug
	}
	return leaf.ID
}

// noteNoWorkReason handles one empty reply from head for leaf: it records the head's
// reason, raises that reason's notice, retires the notices a different answer ends, and
// starts the cap wait on INFLIGHT_CAP. It reports whether the reply named a reason — an
// explained empty answer is not "no work matching this machine" and must not feed that
// streak.
func (f *Fetcher) noteNoWorkReason(head *ServerConnection, leaf CachedLeafInfo, resp *lettucev1.RequestWorkUnitResponse) bool {
	code := noWorkNoticeCode(resp.GetNoWorkReason())
	addr := head.Config.GRPCAddress

	// Every current head reports a bench whatever was asked, so any other answer means
	// the account is not benched there (any more).
	if code != noticeAccountBenched {
		f.notices.Resolve(noticeAccountBenched, head.Name, "")
	}
	// One live reason per head and leaf: a different answer for the same leaf ends the
	// one before it (the head's waiting leaves are kept in step through its status).
	if code != noticeInfeasibleDeadline {
		f.notices.Resolve(noticeInfeasibleDeadline, head.Name, leaf.ID)
	}
	if code == "" {
		f.headStatus.ClearNoWorkFor(addr, leaf.ID)
		f.refreshWaitingNotices(head, "")
		return false
	}

	idle, slots := 0, 0
	if f.idleSlotsFn != nil {
		idle, slots = f.idleSlotsFn()
	}
	msg := noWorkReasonMessage(head.Name, leafNoticeLabel(leaf), resp, idle, slots)
	nw := HeadNoWork{Reason: code, Message: msg, LeafID: leaf.ID, At: f.now()}
	if code != noticeInflightCap && code != noticeAccountBenched {
		nw.Leaf = leafNoticeLabel(leaf)
	}
	f.headStatus.SetNoWork(addr, head.Name, nw)
	f.logger.Info("fetcher: head sent no work, and said why", "server", head.Name, "leaf_slug", leaf.Slug, "reason", code,
		"inflight_cap", resp.GetInflightCap(), "inflight_held", resp.GetInflightHeld(), "idle_slots", idle)

	switch code {
	case noticeInflightCap:
		// Leave the head alone until one of this machine's copies of its units is done.
		head.capWaitHeld = f.heldFromHead(head.Name)
		head.capWaitUntil = f.now().Add(capWaitMax)
		// The cap is only worth the volunteer's attention while it leaves a slot with
		// nothing to run; with every slot busy the head is just bounding the buffer.
		if idle > 0 {
			f.notices.Notify(NoticeWarn, noticeInflightCap, msg, head.Name, "")
		}
	case noticeAccountBenched:
		f.notices.Notify(NoticeWarn, noticeAccountBenched, msg, head.Name, "")
	case noticeInfeasibleDeadline:
		f.notices.Notify(NoticeWarn, noticeInfeasibleDeadline, msg, head.Name, leaf.ID)
	}
	// Already contributed and bench cooldown ask nothing of the volunteer, idle slots
	// or not: the other leaves they could run are theirs to choose, and these wait for
	// other volunteers. They are information, one line per head.
	f.refreshWaitingNotices(head, code)
	return true
}

// refreshWaitingNotices keeps a head's informational notices in step with its waiting
// leaves: one Info notice per reason and head, worded for every leaf the head answers
// with it, refreshed for the reason the head just gave (observed) and resolved once no
// leaf is left with it.
func (f *Fetcher) refreshWaitingNotices(head *ServerConnection, observed string) {
	for _, code := range waitingReasonNotices {
		msg := f.headStatus.WaitingMessage(head.Config.GRPCAddress, code)
		switch {
		case msg == "":
			f.notices.Resolve(code, head.Name, "")
		case code == observed:
			f.notices.Notify(NoticeInfo, code, msg, head.Name, "")
		}
	}
}

// noteHeadServed ends what a head's earlier empty answers said once it sends work.
func (f *Fetcher) noteHeadServed(head *ServerConnection, leaf CachedLeafInfo) {
	head.capWaitUntil = time.Time{}
	f.headStatus.ClearNoWork(head.Config.GRPCAddress)
	for _, c := range headReasonNotices {
		f.notices.Resolve(c, head.Name, "")
	}
	for _, c := range waitingReasonNotices {
		f.notices.Resolve(c, head.Name, "")
	}
	f.notices.Resolve(noticeInfeasibleDeadline, head.Name, leaf.ID)
}

// resolveCapNoticesIfBusy ends every head's cap notice once no slot is left idle: the
// cap no longer costs this machine anything.
func (f *Fetcher) resolveCapNoticesIfBusy() {
	if f.idleSlotsFn == nil {
		return
	}
	if idle, _ := f.idleSlotsFn(); idle == 0 {
		f.notices.Resolve(noticeInflightCap, "", "")
	}
}

// heldFromHead is how many units this machine holds (buffered or running) from head.
func (f *Fetcher) heldFromHead(name string) int {
	if f.heldFromHeadFn == nil {
		return 0
	}
	return f.heldFromHeadFn(name)
}

// capWaiting reports whether head is being left alone after an INFLIGHT_CAP answer,
// releasing it once this machine holds fewer of its units than it did then, or after
// capWaitMax.
func (f *Fetcher) capWaiting(head *ServerConnection) bool {
	if head.capWaitUntil.IsZero() {
		return false
	}
	if f.now().Before(head.capWaitUntil) && f.heldFromHead(head.Name) >= head.capWaitHeld {
		return true
	}
	f.logger.Debug("fetcher: asking a capped head again", "server", head.Name,
		"held_then", head.capWaitHeld, "held_now", f.heldFromHead(head.Name), "timed_out", !f.now().Before(head.capWaitUntil))
	head.capWaitUntil = time.Time{}
	f.notices.Resolve(noticeInflightCap, head.Name, "")
	return false
}

// anyHeadCapWaiting reports whether some head is holding this machine at its cap: that
// head has work for this machine, so the machine is not "getting no work".
func (f *Fetcher) anyHeadCapWaiting() bool {
	if f.multiClient == nil {
		return false
	}
	for _, srv := range f.multiClient.Servers() {
		if f.capWaiting(srv) {
			return true
		}
	}
	return false
}
