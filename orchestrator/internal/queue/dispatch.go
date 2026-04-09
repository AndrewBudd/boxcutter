package queue

import (
	"fmt"
	"log"
	"time"
)

// IdleWorker represents a VM that is idle and available for work.
type IdleWorker struct {
	Name   string
	NodeID string
	Team   string
}

// IdleWorkerFn is a callback that returns the currently idle workers.
type IdleWorkerFn func() []IdleWorker

// AssignFn is a callback that delivers a work assignment to a VM via tapegun inbox.
type AssignFn func(vmName string, messageBody string) error

// Dispatcher runs the queue dispatch and GitHub sync loops.
type Dispatcher struct {
	queue       *Queue
	getIdle     IdleWorkerFn
	assignToVM  AssignFn
	stopCh      chan struct{}
}

// NewDispatcher creates a dispatcher wired to the queue and callback functions.
func NewDispatcher(q *Queue, getIdle IdleWorkerFn, assign AssignFn) *Dispatcher {
	return &Dispatcher{
		queue:      q,
		getIdle:    getIdle,
		assignToVM: assign,
		stopCh:     make(chan struct{}),
	}
}

// Start launches the background sync and dispatch loops.
func (d *Dispatcher) Start() {
	go d.syncLoop()
	go d.assignLoop()
	go d.timeoutLoop()
	go d.completionLoop()
	log.Printf("queue: dispatcher started (sync=%s, assign=%s, timeout=%dm)",
		d.queue.cfg.PollInterval, d.queue.cfg.AssignInterval, d.queue.cfg.TimeoutMinutes)
}

// Stop signals the dispatcher to stop.
func (d *Dispatcher) Stop() {
	close(d.stopCh)
}

// syncLoop periodically fetches new GitHub issues.
func (d *Dispatcher) syncLoop() {
	// Initial sync on startup
	d.queue.SyncGitHubIssues()

	ticker := time.NewTicker(d.queue.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			if _, err := d.queue.SyncGitHubIssues(); err != nil {
				log.Printf("queue: sync error: %v", err)
			}
		}
	}
}

// assignLoop checks for idle workers and assigns queued items to them.
func (d *Dispatcher) assignLoop() {
	ticker := time.NewTicker(d.queue.cfg.AssignInterval)
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
	idleWorkers := d.getIdle()
	if len(idleWorkers) == 0 {
		return
	}

	for _, worker := range idleWorkers {
		// Skip if this worker already has an assigned task
		if existing := d.queue.AssignedTo(worker.Name); existing != nil {
			continue
		}

		item := d.queue.NextQueued(worker.Team)
		if item == nil {
			return // no more work
		}

		if err := d.queue.Assign(item.ID, worker.Name); err != nil {
			log.Printf("queue: assign error: %v", err)
			continue
		}

		msg := FormatAssignmentMessage(item)
		if err := d.assignToVM(worker.Name, msg); err != nil {
			log.Printf("queue: failed to deliver assignment to %s: %v", worker.Name, err)
			// Re-enqueue on delivery failure
			d.queue.Reassign(item.ID)
			continue
		}

		log.Printf("queue: assigned %s (%s) to %s", item.SourceRef, item.Title, worker.Name)
	}
}

// timeoutLoop checks for stale assignments and re-enqueues them.
func (d *Dispatcher) timeoutLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			timedOut := d.queue.TimeoutStale()
			for _, item := range timedOut {
				log.Printf("queue: assignment timed out: %s (was assigned to %s)", item.Title, item.AssignedTo)
			}
		}
	}
}

// completionLoop checks if assigned items' GitHub issues have been closed.
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
	assigned := d.queue.List("assigned")
	for _, item := range assigned {
		if item.Source != "github-issue" || item.SourceRef == "" {
			continue
		}
		repo, number, ok := ParseIssueRef(item.SourceRef)
		if !ok {
			continue
		}
		if CheckIssueClosed(repo, number) {
			d.queue.Complete(item.ID)
			log.Printf("queue: auto-completed %s (%s) — issue closed", item.ID, item.SourceRef)
		}
	}
}

// Stats returns queue statistics.
func (d *Dispatcher) Stats() QueueStats {
	items := d.queue.List("")
	var stats QueueStats
	for _, item := range items {
		switch item.Status {
		case "queued":
			stats.Queued++
		case "assigned":
			stats.Assigned++
			age := time.Since(*item.AssignedAt)
			if age > stats.OldestAssignment {
				stats.OldestAssignment = age
			}
		case "completed":
			stats.Completed++
		case "failed":
			stats.Failed++
		}
	}
	stats.Total = len(items)
	return stats
}

// QueueStats is a summary of queue state.
type QueueStats struct {
	Total            int           `json:"total"`
	Queued           int           `json:"queued"`
	Assigned         int           `json:"assigned"`
	Completed        int           `json:"completed"`
	Failed           int           `json:"failed"`
	OldestAssignment time.Duration `json:"oldest_assignment"`
}

func (s QueueStats) String() string {
	return fmt.Sprintf("total=%d queued=%d assigned=%d completed=%d failed=%d",
		s.Total, s.Queued, s.Assigned, s.Completed, s.Failed)
}
