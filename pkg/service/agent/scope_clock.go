package agent

import (
	"context"
	"maps"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// TestClock is an optional extension of AgentHooks, for a build that freezes
// the clock of the run's processes. Under the proven switch, a replay moves
// that clock to each test's recorded begin while the test is open, and back
// to the run's start time when it ends (design v14 §3.4): a test sees the
// instant its recording saw, whatever ran before it.
type TestClock interface {
	// BeginTestClock moves the run's clock to at, when test began in its
	// recording. A zero at (a recording made before keploy kept it) keeps the
	// run's start time.
	BeginTestClock(ctx context.Context, test string, at time.Time) error
	// EndTestClock moves the run's clock back to its start time.
	EndTestClock(ctx context.Context, test string) error
}

// TimeFiler is an optional extension of AgentHooks: the clock record the
// run's processes read through KEPLOY_TIME_FILE.
type TimeFiler interface {
	TimeFile() string
}

// TimeFile is the clock record the run's processes read, which a test
// process's keploy integration exports as KEPLOY_TIME_FILE; "" when this
// agent does not freeze the run.
func TimeFile() string {
	if f, ok := ActiveHooks.(TimeFiler); ok {
		return f.TimeFile()
	}
	return ""
}

// SetTestBegins installs when each test of the replayed recording began, by
// test name, and anchors the run's clock at start, the earliest of them. The
// CLI installs them for a replay session, like the scope table.
func (a *Agent) SetTestBegins(ctx context.Context, start time.Time, begins map[string]time.Time) error {
	a.scopeMu.Lock()
	a.testBegins = maps.Clone(begins)
	a.scopeMu.Unlock()
	if start.IsZero() {
		return nil
	}
	return ActiveHooks.SetFreezeAnchor(ctx, start)
}

// testClock is the build's TestClock when the proven switch is on.
func testClock() (TestClock, bool) {
	if !provenswitch.On() {
		return nil, false
	}
	c, ok := ActiveHooks.(TestClock)
	return c, ok
}

// beginTestClock moves the run's clock to test's recorded begin. A clock that
// cannot be moved fails the begin: the test would run on another clock than
// its recording's, and pass or fail for that.
func (a *Agent) beginTestClock(ctx context.Context, test string) error {
	c, ok := testClock()
	if !ok {
		return nil
	}
	a.scopeMu.Lock()
	at := a.testBegins[test]
	a.scopeMu.Unlock()
	return c.BeginTestClock(ctx, test, at)
}

// endTestClock moves the run's clock back to its start time.
func (a *Agent) endTestClock(ctx context.Context, test string) error {
	c, ok := testClock()
	if !ok {
		return nil
	}
	return c.EndTestClock(ctx, test)
}
