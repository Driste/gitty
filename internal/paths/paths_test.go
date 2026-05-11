package paths

import "testing"

func TestLocal(t *testing.T) {
	cases := []struct {
		name        string
		apiFullPath string
		configRoot  string
		want        string
	}{
		{"prefix-trim", "tenant/images/api", "tenant", "images/api"},
		{"empty-root-passthrough", "tenant/images/api", "", "tenant/images/api"},
		{"exact-match", "tenant", "tenant", ""},
		{"trailing-slash-on-root", "tenant/images/api", "tenant/", "images/api"},
		{"non-prefix-passthrough", "other/repo", "tenant", "other/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Local(tc.apiFullPath, tc.configRoot)
			if got != tc.want {
				t.Fatalf("Local(%q, %q) = %q, want %q", tc.apiFullPath, tc.configRoot, got, tc.want)
			}
		})
	}
}
