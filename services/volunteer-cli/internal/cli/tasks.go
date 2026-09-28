package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func newTasksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "Act on running tasks (restart)",
		Args:  noStrayArgs,
		// A non-runnable parent never reaches its Args constraint, so the
		// help is served from RunE instead.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newTasksRestartCmd())
	return cmd
}

func newTasksRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart <task-id>",
		Short: "Restart a running task so it runs with the current settings",
		Long: `Stop a running task and run its work unit again from the start with the
settings in force now.

A task keeps the cores it was given, and the other settings it started with,
until it finishes: a change to the CPU limit or to a leaf's cores applies to
tasks started afterwards. Restarting is how a task already running picks up
the change. The work it has done so far is lost unless its leaf saves
checkpoints, in which case it continues from the last one. Its deadline keeps
counting from when it first started.

The task id is the ID column of ` + "`lettuce-volunteer status`" + ` (the first
characters are enough when they match one task). To give a task's unit back
to its head instead, use Abort in the app.`,
		Args: cobra.ExactArgs(1),
		RunE: runTasksRestart,
	}
}

func runTasksRestart(cmd *cobra.Command, args []string) error {
	var sr struct {
		ActiveTasks []struct {
			WorkUnitID string `json:"work_unit_id"`
			LeafName   string `json:"leaf_name"`
		} `json:"active_tasks"`
	}
	if err := managementGet(cfg.DataDir, "/api/v1/status", &sr); err != nil {
		return fmt.Errorf("cannot reach the daemon (%v); a task can only be restarted while it runs", err)
	}
	want := strings.ToLower(strings.TrimSpace(args[0]))
	var matches []int
	for i, t := range sr.ActiveTasks {
		if want != "" && strings.HasPrefix(strings.ToLower(t.WorkUnitID), want) {
			matches = append(matches, i)
		}
	}
	switch {
	case len(matches) == 0:
		return fmt.Errorf("no running task has an id starting %q; `lettuce-volunteer status` lists the running tasks", args[0])
	case len(matches) > 1:
		return fmt.Errorf("%d running tasks have ids starting %q; give more of the id", len(matches), args[0])
	}
	task := sr.ActiveTasks[matches[0]]
	err := managementSend(cfg.DataDir, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(task.WorkUnitID)+"/restart", nil, nil)
	if err != nil {
		if errors.Is(err, errDaemonAbsent) {
			return fmt.Errorf("cannot reach the daemon (%v)", err)
		}
		return err
	}
	name := task.LeafName
	if name == "" {
		name = "the"
	}
	fmt.Printf("Restarting %s task %s: it stops now and starts again with the current settings.\n", name, shortID(task.WorkUnitID))
	return nil
}

// shortID is the first eight characters of a work unit id: what `status`
// shows and `tasks restart` accepts.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
