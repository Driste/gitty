// Package paths resolves GitLab namespace paths to local workspace
// directories. It is a leaf package with no internal dependencies.
package paths

import "strings"

// Local returns the local directory path corresponding to apiFullPath
// when the workspace is anchored at configRoot. The result is always
// relative (no leading slash). If configRoot is empty, apiFullPath is
// returned unchanged. If configRoot is a prefix of apiFullPath, that
// prefix (and any single leading slash that follows it) is trimmed.
func Local(apiFullPath, configRoot string) string {
	if configRoot == "" {
		return apiFullPath
	}
	rel := strings.TrimPrefix(apiFullPath, configRoot)
	rel = strings.TrimPrefix(rel, "/")
	return rel
}
