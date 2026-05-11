package sync

import "errors"

// ErrNoToken is returned by ResolveToken when no token source is set.
var ErrNoToken = errors.New("Error: A token (via --token flag, GITLAB_TOKEN, or CI_JOB_TOKEN env var) is required.")

// ResolveToken returns the GitLab access token to use for this invocation.
// Resolution order is fixed by Constitution Principle III: explicit value
// first, then GITLAB_TOKEN, then CI_JOB_TOKEN. The lookup parameter is
// injected so tests can drive token resolution without setting process
// environment variables.
func ResolveToken(explicit string, lookup func(string) string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if t := lookup("GITLAB_TOKEN"); t != "" {
		return t, nil
	}
	if t := lookup("CI_JOB_TOKEN"); t != "" {
		return t, nil
	}
	return "", ErrNoToken
}
