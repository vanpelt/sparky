package fleet

// White-box hooks for the package's external tests.
//
// They live here rather than as real methods because they answer questions
// only a test asks. LiveStreams in particular must not become an API: a
// registry size is a fact about this process's plumbing, and exporting it would
// invite something in the control plane to branch on it.

// LiveStreams is how many tunneled conns this fleet has handed out and not yet
// seen closed.
func LiveStreams(f *Fleet) int {
	f.smu.Lock()
	defer f.smu.Unlock()
	return len(f.streams)
}
