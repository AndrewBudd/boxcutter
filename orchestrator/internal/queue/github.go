package queue

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// GitHubIssue is the subset of issue fields returned by `gh issue list --json`.
type GitHubIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	State string `json:"state"`
	URL   string `json:"url"`
}

// SyncGitHubIssues queries GitHub for open issues with the configured label
// and enqueues any that aren't already in the queue.
func (q *Queue) SyncGitHubIssues() (int, error) {
	if q.cfg.Repo == "" || q.cfg.Label == "" {
		return 0, nil
	}

	issues, err := fetchGitHubIssues(q.cfg.Repo, q.cfg.Label)
	if err != nil {
		return 0, fmt.Errorf("github sync: %w", err)
	}

	added := 0
	for _, issue := range issues {
		ref := fmt.Sprintf("%s#%d", q.cfg.Repo, issue.Number)

		var labelNames []string
		for _, l := range issue.Labels {
			labelNames = append(labelNames, l.Name)
		}

		priority := PriorityFromLabels(labelNames, q.cfg.PriorityLabels)

		item := &WorkItem{
			Source:    "github-issue",
			SourceRef: ref,
			Title:    issue.Title,
			Body:     issue.Body,
			Priority: priority,
			Labels:   labelNames,
		}

		if err := q.Enqueue(item); err != nil {
			log.Printf("queue: failed to enqueue %s: %v", ref, err)
			continue
		}
		added++
	}

	if added > 0 {
		log.Printf("queue: synced %d new issues from %s (label: %s)", added, q.cfg.Repo, q.cfg.Label)
	}
	return added, nil
}

// fetchGitHubIssues calls `gh issue list` to get open issues with a label.
func fetchGitHubIssues(repo, label string) ([]GitHubIssue, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--repo", repo,
		"--label", label,
		"--state", "open",
		"--json", "number,title,body,labels,state,url",
		"--limit", "50")

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}

	var issues []GitHubIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing gh output: %w", err)
	}
	return issues, nil
}

// CheckIssueClosed checks if a GitHub issue is closed (for completion detection).
func CheckIssueClosed(repo string, number int) bool {
	ref := fmt.Sprintf("%s#%d", repo, number)
	cmd := exec.Command("gh", "issue", "view", ref, "--json", "state", "--jq", ".state")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "CLOSED"
}

// ParseIssueRef extracts repo and issue number from a source_ref like "owner/repo#42".
func ParseIssueRef(ref string) (repo string, number int, ok bool) {
	parts := strings.SplitN(ref, "#", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	_, err := fmt.Sscanf(parts[1], "%d", &number)
	if err != nil {
		return "", 0, false
	}
	return parts[0], number, true
}
