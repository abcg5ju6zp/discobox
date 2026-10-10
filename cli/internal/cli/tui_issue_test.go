package cli

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/github"
)

// Anyone who can comment on an issue writes its text, so none of it reaches
// the console's screen as something a terminal would act on.
func TestAnIssueIsMadeSafeToDraw(t *testing.T) {
	t.Parallel()
	evil := "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07\x1b[2J\u202e"
	issue := tuiIssue(github.Issue{
		Title:  "title " + evil,
		Body:   "line one\r\nline two " + evil,
		Labels: []github.Label{{Name: evil, Color: "ffffff"}},
		Timeline: []github.Event{
			{Kind: "commented", Actor: evil, Body: "comment\r\n" + evil},
			{Kind: "cross-referenced", Source: &github.Ref{Title: evil}},
		},
	})
	for name, value := range map[string]string{
		"title": issue.Title, "body": issue.Body, "label": issue.Labels[0].Name,
		"actor": issue.Timeline[0].Actor, "comment": issue.Timeline[0].Body,
		"reference": issue.Timeline[1].Source.Title,
	} {
		if strings.ContainsAny(value, "\x1b\x07\u202e\r") {
			t.Errorf("the %s reaches the screen raw: %q", name, value)
		}
	}
	if !strings.Contains(issue.Body, "line one\nline two") {
		t.Errorf("the body lost its lines: %q", issue.Body)
	}
}

// A pull request's own text is a stranger's too: a branch name, a check's name
// and its URL, a file's path and its diff, a review thread.
func TestAPullRequestIsMadeSafeToDraw(t *testing.T) {
	t.Parallel()
	evil := "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07\x1b[2J\u202e"
	issue := tuiIssue(github.Issue{
		PullRequest: true,
		Pull: &github.Pull{
			Head: evil, Base: evil, MergedBy: evil, Mergeable: evil,
			Reviewers: []github.Reviewer{{Name: evil, State: evil}},
			Checks:    []github.Check{{Name: evil, State: evil, URL: "https://ci.example/\x1b[2J"}},
			Files:     []github.File{{Path: evil, Previous: evil, Status: evil, Patch: "+\tone\r\n-two " + evil}},
		},
		Timeline: []github.Event{{
			Kind: "reviewed", Review: evil, Message: evil,
			Threads: []github.Thread{{Path: evil, Hunk: "@@\n+" + evil, Comments: []github.ThreadComment{{Author: evil, Body: evil}}}},
		}},
	})
	pull, review := issue.Pull, issue.Timeline[0]
	if pull == nil || len(review.Threads) != 1 {
		t.Fatalf("the pull request did not come through: %+v", issue)
	}
	file, thread := pull.Files[0], review.Threads[0]
	for name, value := range map[string]string{
		"head": pull.Head, "base": pull.Base, "merger": pull.MergedBy, "mergeable": pull.Mergeable,
		"reviewer": pull.Reviewers[0].Name, "reviewer state": pull.Reviewers[0].State,
		"check": pull.Checks[0].Name, "check state": pull.Checks[0].State, "check URL": pull.Checks[0].URL,
		"path": file.Path, "previous path": file.Previous, "status": file.Status, "patch": file.Patch,
		"review": review.Review, "message": review.Message,
		"thread path": thread.Path, "hunk": thread.Hunk,
		"thread author": thread.Comments[0].Author, "thread comment": thread.Comments[0].Body,
	} {
		if strings.ContainsAny(value, "\x1b\x07\u202e\r") {
			t.Errorf("the %s reaches the screen raw: %q", name, value)
		}
	}
	if !strings.HasPrefix(file.Patch, "+\tone\n-two ") {
		t.Errorf("the patch lost its lines or its tabs: %q", file.Patch)
	}
}
