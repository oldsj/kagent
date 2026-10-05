package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const maxDetail = 160

// failureMessage turns a bootstrap error into a short public message. It names
// the likely cause for the common cases and otherwise quotes git's first
// stderr line, which carries no credentials because the gateway injects them
// outside the actor.
func failureMessage(request Request, err error) string {
	const prefix = "Workspace bootstrap failed: "
	var gitErr *gitError
	switch {
	case errors.Is(err, errNoGit):
		return prefix + "git is not installed in the runtime image."
	case errors.Is(err, context.DeadlineExceeded):
		return prefix + "timed out cloning " + request.Repo + "."
	case errors.Is(err, errForeignGit):
		return prefix + "the workspace already has a Git repository that the bootstrap did not create, so it was left untouched. Move or remove it, or start a Session without a workspace."
	case errors.Is(err, errRefNotFound):
		return fmt.Sprintf("%sref %q was not found in %s.", prefix, request.Ref, request.Repo)
	case errors.As(err, &gitErr):
		lower := strings.ToLower(gitErr.Stderr)
		switch {
		case containsAny(lower, "authentication failed", "could not read username", "returned error: 401", "returned error: 403", "repository not found", "invalid credentials"):
			return prefix + request.Repo + " was not found or the credential was rejected."
		case containsAny(lower, "could not resolve host", "failed to connect", "connection refused", "connection timed out", "ssl", "unable to access"):
			return prefix + "could not reach the repository host; check the agent's Git origins."
		case gitErr.Step == "check-ref-format":
			return prefix + "the ref or branch name is invalid."
		}
		return fmt.Sprintf("%sgit %s failed%s.", prefix, gitErr.Step, firstLine(gitErr.Stderr))
	}
	return prefix + "could not prepare the checkout."
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// firstLine returns ": <first line>" truncated, or "" for empty stderr.
func firstLine(stderr string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if len(line) > maxDetail {
		line = line[:maxDetail] + "…"
	}
	return ": " + line
}
