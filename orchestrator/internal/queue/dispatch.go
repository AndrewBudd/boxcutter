package queue

import (
	"fmt"
	"log"
	"time"

	"github.com/AndrewBudd/boxcutter/orchestrator/internal/node"
	"github.com/AndrewBudd/boxcutter/orchestrator/internal/state"
)

// Dispatcher runs background loops that sync GitHub issues, assign work to
// idle workers, detect timeouts, and check for completions.
type Dispatcher struct {
	queue  *Queue
	state  *state.Store
	repo   string
	label  string
	stopCh chan struct{}
}

// NewDispatcher creates a Dispatcher. Call Start() to begin background loops.
func NewDispatcher(q *Queue, st *state.Store, repo, label string) *Dispatcher {
	return &Dispatcher{
		queue:  q,
		state:  st,
		repo:   repo,
		label:  label,
		stopCh: make(chan struct{}),
	}
}

// Queue returns the underlying Queue for direct access (API handlers).
func (d *Dispatcher) Queue() *Queue {
	return d.queue
}

// Start launches the four background loops.
func (d *Dispatcher) Start() {
	go d.syncLoop()
	go d.assignLoop()
	go d.timeoutLoop()
	go d.completionLoop()
}

// Stop signals all loops to exit.
func (d *Dispatcher) Stop() {
	close(d.stopCh)
}

// syncLoop periodically fetches labeled GitHub issues and enqueues new ones.
func (d *Dispatcher) syncLoop() {
	// Initial sync on startup
	if n, err := SyncFromGitHub(d.queue, d.repo, d.label); err == nil && n > 0 {
		log.Printf("Work queue: synced %d new issues from GitHub", n)
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			n, err := SyncFromGitHub(d.queue, d.repo, d.label)
			if err != nil {
				log.Printf("Work queue sync error: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("Work queue: synced %d new issues from GitHub", n)
			}
		}
	}
}

// assignLoop checks for idle workers and assigns the highest-priority queued item.
func (d *Dispatcher) assignLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.tryAssign()
		}
	}
}

func (d *Dispatcher) tryAssign() {
	queued, err := d.queue.ListByStatus(StatusQueued)
	if err != nil || len(queued) == 0 {
		return
	}

	// Find an idle worker
	idle := d.findIdleWorker()
	if idle == "" {
		return
	}

	item := queued[0] // highest priority
	if err := d.queue.Assign(item.ID, idle); err != nil {
		log.Printf("Work queue: assign %d to %s failed: %v", item.ID, idle, err)
		return
	}

	// Deliver assignment via tapegun inbox
	d.sendAssignment(idle, item)
	log.Printf("Work queue: assigned %s (%s) to %s", item.SourceRef, item.Title, idle)
}

// findIdleWorker returns the name of an idle worker VM, or "" if none.
func (d *Dispatcher) findIdleWorker() string {
	vms := d.state.ListVMs()
	for _, vm := range vms {
		if vm.Status != "running" {
			continue
		}
		n := d.state.GetNode(vm.NodeID)
		if n == nil || n.APIAddr == "" {
			continue
		}
		fc := node.NewFastClient(n.APIAddr)
		acts := fc.GetAllActivity()
		for _, act := range acts {
			if act.VMID == vm.Name && act.LastActivity != nil &&
				act.LastActivity.Status == "idle" && act.PendingMessages == 0 {
				return vm.Name
			}
		}
	}
	return ""
}

// sendAssignment delivers a work assignment to a VM via tapegun inbox.
func (d *Dispatcher) sendAssignment(vmName string, item *WorkItem) {
	vm := d.state.GetVM(vmName)
	if vm == nil {
		return
	}
	n := d.state.GetNode(vm.NodeID)
	if n == nil || n.APIAddr == "" {
		return
	}

	body := fmt.Sprintf("TASK ASSIGNMENT\nIssue: %s\nTitle: %s\nPriority: %d\n\n%s\n\nPlease work on this issue. When done, create a PR and close the issue.",
		item.SourceRef, item.Title, item.Priority, item.Body)

	msg := &node.TapegunMessage{
		ID:       fmt.Sprintf("wq-%d-%d", item.ID, time.Now().UnixNano()),
		From:     "work-queue",
		Body:     body,
		Priority: "normal",
	}

	client := node.NewClient(n.APIAddr)
	if err := client.SendTapegunMessage(vmName, msg); err != nil {
		log.Printf("Work queue: failed to send assignment to %s: %v", vmName, err)
	}
}

// timeoutLoop checks for stale assignments and re-queues or fails them.
func (d *Dispatcher) timeoutLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.checkTimeouts()
		}
	}
}

func (d *Dispatcher) checkTimeouts() {
	stale, err := d.queue.StaleAssignments(30 * time.Minute)
	if err != nil {
		log.Printf("Work queue: timeout check error: %v", err)
		return
	}
	for _, item := range stale {
		if item.Attempts >= item.MaxAttempts {
			d.queue.Fail(item.ID)
			log.Printf("Work queue: item %d (%s) failed after %d attempts", item.ID, item.SourceRef, item.Attempts)
		} else {
			d.queue.Reassign(item.ID)
			log.Printf("Work queue: item %d (%s) timed out, re-queued (attempt %d/%d)",
				item.ID, item.SourceRef, item.Attempts, item.MaxAttempts)
		}
	}
}

// completionLoop checks if assigned items have been completed (issue closed).
func (d *Dispatcher) completionLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.checkCompletions()
		}
	}
}

func (d *Dispatcher) checkCompletions() {
	assigned, err := d.queue.ListByStatus(StatusAssigned)
	if err != nil {
		return
	}
	for _, item := range assigned {
		if item.Source != "github" {
			continue
		}
		repo, number, ok := ParseIssueNumber(item.SourceRef)
		if !ok {
			continue
		}
		if IsIssueClosed(repo, number) {
			d.queue.Complete(item.ID)
			log.Printf("Work queue: item %d (%s) completed — issue closed", item.ID, item.SourceRef)
		}
	}
}
