package gitexec

import (
	"context"
	"testing"
)

// fakeRunner satisfies Runner without invoking the git binary. It records
// every Run call into Calls so tests can assert ordering and arguments,
// and returns whatever Stderr/Err the test author configured.
type fakeRunner struct {
	Calls  []FakeCall
	Stderr []byte
	Err    error
}

type FakeCall struct {
	Dir  string
	Args []string
}

func (f *fakeRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	f.Calls = append(f.Calls, FakeCall{Dir: dir, Args: append([]string{}, args...)})
	return f.Stderr, f.Err
}

// TestRunnerInterfaceContract proves any code accepting Runner can be
// driven by a hand-written fake — no `git` binary required.
func TestRunnerInterfaceContract(t *testing.T) {
	var r Runner = &fakeRunner{Stderr: []byte("fatal: nope\n")}
	ctx := context.Background()

	stderr, err := r.Run(ctx, "/some/dir", "clone", "git@host:foo.git", "foo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stderr) != "fatal: nope\n" {
		t.Errorf("stderr passthrough wrong: %q", stderr)
	}

	stderr, err = r.Run(ctx, "/some/dir/foo", "pull")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stderr) != "fatal: nope\n" {
		t.Errorf("stderr passthrough wrong: %q", stderr)
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
