package daemon

import (
	"fmt"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// The preview of what would run together on this machine under the current
// settings: the cores each task of each enabled leaf would be given and how
// many would run at once, for each leaf on its own and for all of them
// together. It is worked out by the very arithmetic the slot filler starts
// tasks with — the budget ledger's admission questions (fits) and the grant
// (grantFrom), placing a buffer's worth of units on an idle machine one after
// another — so what it shows is what the scheduler does, not a second
// opinion that could drift from it. What only the moment can say (free RAM,
// free disk, a running task's grant) is left out: the preview answers "with
// these settings", not "right now".

// PreviewTask is one task the preview starts: its leaf, the head serving it
// and the cores it would be given.
type PreviewTask struct {
	LeafID   string `json:"leaf_id"`
	LeafName string `json:"leaf_name"`
	Head     string `json:"head"`
	Cores    int    `json:"cores"`
}

// PreviewLeaf is one enabled leaf run on its own: how many of its tasks
// would run at once and the cores each would be given; or why none can start.
type PreviewLeaf struct {
	LeafID    string `json:"leaf_id"`
	LeafName  string `json:"leaf_name"`
	Head      string `json:"head"`
	Tasks     []int  `json:"tasks"` // the cores of each task started, in order
	CoresUsed int    `json:"cores_used"`
	// CannotStart says why no task of the leaf can start here ("" when some
	// can).
	CannotStart string `json:"cannot_start,omitempty"`
}

// RunPreview is the preview: the CPU limit, each enabled leaf on its own,
// and all of them together — the tasks started when the buffer holds work of
// every leaf in turn, the cores they use, and the task that would then wait
// for cores (none when nothing does).
type RunPreview struct {
	CPULimit        int           `json:"cpu_limit"`
	Alone           []PreviewLeaf `json:"alone"`
	Together        []PreviewTask `json:"together"`
	TogetherCores   int           `json:"together_cores"`
	WaitingForCores *PreviewTask  `json:"waiting_for_cores,omitempty"`
}

// RunPreview works the preview out from the current settings and the enabled
// leafs' declarations.
func (d *Daemon) RunPreview() RunPreview {
	p := RunPreview{CPULimit: d.HostCPUBudgetCores(), Alone: []PreviewLeaf{}, Together: []PreviewTask{}}
	var runnable []headLeaf
	for _, hl := range d.enabledLeafsByHead() {
		if d.leafNeedsAbsentGPU(hl.leaf) {
			continue
		}
		entry := PreviewLeaf{LeafID: hl.leaf.ID, LeafName: previewName(hl.leaf), Head: hl.head, Tasks: []int{}}
		if why := d.cannotStartReason(hl.leaf); why != "" {
			entry.CannotStart = why
			p.Alone = append(p.Alone, entry)
			continue
		}
		started, _ := d.simulateStarts(d.previewUnits([]headLeaf{hl}))
		for _, s := range started {
			entry.Tasks = append(entry.Tasks, s.Cores)
			entry.CoresUsed += s.Cores
		}
		p.Alone = append(p.Alone, entry)
		runnable = append(runnable, hl)
	}
	started, waiting := d.simulateStarts(d.previewUnits(runnable))
	p.Together = started
	for _, s := range started {
		p.TogetherCores += s.Cores
	}
	p.WaitingForCores = waiting
	return p
}

// previewName is how the preview names a leaf.
func previewName(leaf CachedLeafInfo) string {
	if name := leafNoticeLabel(leaf); name != "" {
		return name
	}
	return leaf.ID
}

// previewUnits is a buffer's worth of units shaped like the leafs'
// declarations: as many rounds as the machine has slots, one unit of each
// leaf per round, in the order given.
func (d *Daemon) previewUnits(leafs []headLeaf) []*runtime.WorkUnit {
	var units []*runtime.WorkUnit
	for round := 0; round < d.maxSlots(); round++ {
		for _, hl := range leafs {
			wu := leafShapeUnit(hl.leaf)
			wu.ID = fmt.Sprintf("preview-%s-%d", hl.leaf.ID, round)
			wu.SourceHead = hl.head
			units = append(units, wu)
		}
	}
	return units
}

// simulateStarts places units on an idle machine in order, as the slot
// filler starts buffered units: a unit that fits what is free starts with the
// grant the slot filler would give it (grantFrom, setting cores aside for the
// units behind it that could start too); one refused for another budget
// (memory, a GPU, a running cap) is passed over; the first refused only for
// cores is where it stops, because the slot filler then holds the free cores
// for that unit rather than let narrower ones take them. It returns the tasks
// started and that waiting unit (nil when none).
func (d *Daemon) simulateStarts(units []*runtime.WorkUnit) (started []PreviewTask, waiting *PreviewTask) {
	started = []PreviewTask{}
	ledger := d.emptyLedger()
	for i, wu := range units {
		if !ledger.fits(d, wu) {
			if ledger.fitsBarCores(d, wu) {
				minCores, _ := d.unitCoreRange(wu)
				waiting = &PreviewTask{LeafID: wu.LeafID, LeafName: d.previewLeafName(wu.LeafID), Head: wu.SourceHead, Cores: minCores}
				return started, waiting
			}
			continue
		}
		grant := d.grantFrom(ledger, wu, units[i+1:])
		cores := grant.Cores
		if cores <= 0 {
			cores, _ = d.unitCoreRange(wu)
		}
		ledger.take(d, wu, cores)
		started = append(started, PreviewTask{LeafID: wu.LeafID, LeafName: d.previewLeafName(wu.LeafID), Head: wu.SourceHead, Cores: cores})
	}
	return started, nil
}

// previewLeafName names a leaf by id for the preview.
func (d *Daemon) previewLeafName(leafID string) string {
	if l, ok := d.cachedLeaf(leafID); ok {
		return previewName(l)
	}
	return leafID
}

// fitsBarCores reports whether wu would fit the ledger if cores were no
// object: whether cores are all it is short of.
func (l budgetLedger) fitsBarCores(d *Daemon, wu *runtime.WorkUnit) bool {
	c := l.clone()
	c.hostCores, c.containerCores = noBound, noBound
	return c.fits(d, wu)
}

// DescribeRunPreview renders the preview in a few lines for `doctor`, the tasks
// that start together grouped by leaf and cores and each figure of a leaf on
// its own said once, so a large machine's preview stays a line:
//
//	together: GREP × 1 · 2 cores, Beyblade × 2 · 1 core each — 4 of 4 cores
//	alone: GREP 2 at once, 2 cores each; Wide 66 at once: 62 × 4 cores, 4 × 2 cores
func DescribeRunPreview(p RunPreview) (together, alone string) {
	type group struct {
		name         string
		cores, count int
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, t := range p.Together {
		key := fmt.Sprintf("%s/%s/%d", t.Head, t.LeafID, t.Cores)
		if g := byKey[key]; g != nil {
			g.count++
			continue
		}
		g := &group{name: t.LeafName, cores: t.Cores, count: 1}
		byKey[key] = g
		groups = append(groups, g)
	}
	var parts []string
	for _, g := range groups {
		part := fmt.Sprintf("%s × %d · %s", g.name, g.count, plural(g.cores, "core"))
		if g.count > 1 {
			part += " each"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		together = "nothing can start"
	} else {
		together = strings.Join(parts, ", ")
		if p.CPULimit > 0 {
			together += fmt.Sprintf(" — %d of %d cores", p.TogetherCores, p.CPULimit)
		}
		if w := p.WaitingForCores; w != nil {
			together += fmt.Sprintf("; the next %s task, which needs %s, would wait for them", w.LeafName, plural(w.Cores, "core"))
		}
	}
	var leafs []string
	for _, a := range p.Alone {
		switch {
		case a.CannotStart != "":
			leafs = append(leafs, a.LeafName+" cannot start here")
		case len(a.Tasks) == 0:
			leafs = append(leafs, a.LeafName+" none")
		default:
			leafs = append(leafs, fmt.Sprintf("%s %d at once%s", a.LeafName, len(a.Tasks), coreFigures(a.Tasks)))
		}
	}
	return together, strings.Join(leafs, "; ")
}

// coreFigures says the cores of the tasks of one leaf, to follow "N at once":
// ", 2 cores each", or when they differ each figure once with how many tasks
// get it, in the order they start (": 62 × 4 cores, 4 × 2 cores").
func coreFigures(tasks []int) string {
	var figures []int
	count := map[int]int{}
	for _, c := range tasks {
		if count[c] == 0 {
			figures = append(figures, c)
		}
		count[c]++
	}
	if len(figures) == 1 {
		return ", " + plural(figures[0], "core") + " each"
	}
	parts := make([]string, len(figures))
	for i, c := range figures {
		parts[i] = fmt.Sprintf("%d × %s", count[c], plural(c, "core"))
	}
	return ": " + strings.Join(parts, ", ")
}
