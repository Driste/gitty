package gitexec

import "testing"

// fakeRunner satisfies Runner without invoking the git binary. It records
// every Run call into Calls so tests can assert ordering and arguments.
type fakeRunner struct {
	Calls []FakeCall
	Err   error
}

type FakeCall struct {
	Dir  string
	Args []string
}

func (f *fakeRunner) Run(dir string, args ...string) error {
	f.Calls = append(f.Calls, FakeCall{Dir: dir, Args: append([]string{}, args...)})
	return f.Err
}

// TestRunnerInterfaceContract proves that any code accepting Runner can be
// driven by a hand-written fake — no `git` binary required. This is the
// executable example future contributors copy when adding tests for code
// paths that invoke git.
func TestRunnerInterfaceContract(t *testing.T) {
	var r Runner = &fakeRunner{}

	if err := r.Run("/some/dir", "clone", "git@host:foo.git", "foo"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := r.Run("/some/dir/foo", "pull"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	fr := r.(*fakeRunner)
	if len(fr.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(fr.Calls))
	}
	if fr.Calls[0].Dir != "/some/dir" || fr.Calls[0].Args[0] != "clone" {
		t.Errorf("call 0 wrong: %+v", fr.Calls[0])
	}
	if fr.Calls[1].Dir != "/some/dir/foo" || fr.Calls[1].Args[0] != "pull" {
		t.Errorf("call 1 wrong: %+v", fr.Calls[1])
	}
}
