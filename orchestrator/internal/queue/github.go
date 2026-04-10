package queue

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
)

// GitHubLabel is a label on a GitHub issue.
type GitHubLabel struct {
	Name string `json:"name"`
}

// GitHubIssue is a GitHub issue as returned by `gh issue list --json`.
type GitHubIssue struct {
	Number int           `json:"number"`
	Title  string        `json:"title"`
	Body   string        `json:"body"`
	Labels []GitHubLabel `json:"labels"`
	State  string        `json:"state"`
}

// FetchGitHubIssues calls `gh issue list` for issues matching the given label.
func FetchGitHubIssues(repo, label string) ([]GitHubIssue, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--repo", repo,
		"--label", label,
		"--state", "open",
		"--json", "number,title,body,labels",
		"--limit", "100")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}
	var issues []GitHubIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing gh output: %w", err)
	}
	return issues, nil
}

// IsIssueClosed checks whether a GitHub issue is closed.
func IsIssueClosed(repo string, number int) bool {
	cmd := exec.Command("gh", "issue", "view",
		"--repo", repo,
		fmt.Sprintf("%d", number),
		"--json", "state")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var result struct {
		State string `json:"state"`
	}
	if json.Unmarshal(out, &result) != nil {
		return false
	}
	return strings.EqualFold(result.State, "CLOSED")
}

// ParseIssueNumber extracts the issue number from a source_ref like "owner/repo#42".
func ParseIssueNumber(sourceRef string) (repo string, number int, ok bool) {
	idx := strings.LastIndex(sourceRef, "#")
	if idx < 0 {
		return "", 0, false
	}
	repo = sourceRef[:idx]
	n, err := strconv.Atoi(sourceRef[idx+1:])
	if err != nil {
		return "", 0, false
	}
	return repo, n, true
}

// SyncFromGitHub fetches issues from GitHub and adds new ones to the queue.
// Returns the count of newly added items.
func SyncFromGitHub(q *Queue, repo, label string) (int, error) {
	issues, err := FetchGitHubIssues(repo, label)
	if err != nil {
		log.Printf("GitHub sync error: %v", err)
		return 0, err
	}
	return q.SyncGitHubIssues(issues, repo)
}
