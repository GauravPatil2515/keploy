package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// clockHooks is a build's AgentHooks that freezes the run's clock: it notes
// each move it is asked to make.
type clockHooks struct {
	AgentHook
	moves  []string
	anchor time.Time
	err    error
	file   string
}

func (h *clockHooks) BeginTestClock(_ context.Context, test string, at time.Time) error {
	h.moves = append(h.moves, "begin "+test+" "+at.UTC().Format(time.RFC3339))
	return h.err
}

func (h *clockHooks) EndTestClock(_ context.Context, test string) error {
	h.moves = append(h.moves, "end "+test)
	return h.err
}

func (h *clockHooks) SetFreezeAnchor(_ context.Context, anchor time.Time) error {
	h.anchor = anchor
	return nil
}

func (h *clockHooks) TimeFile() string { return h.file }

func useHooks(t *testing.T, h AgentHooks) {
	t.Helper()
	prev := ActiveHooks
	RegisterHooks(h)
	t.Cleanup(func() { RegisterHooks(prev) })
}

// Under the proven switch, a replayed test's begin moves the run's clock to
// when it began in its recording, and its end moves it back; a test the
// recording kept no begin for keeps the start time (a zero instant). The
// earliest begin anchors the run's clock.
func TestReplayMovesTheClockToEachTestsRecordedBegin(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{}
	useHooks(t, h)
	a := newReplayAgent()
	ctx := context.Background()
	b1 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	b2 := b1.Add(time.Minute)
	require.NoError(t, a.SetTestBegins(ctx, b1, map[string]time.Time{"pkg::TestA": b2, "pkg::TestB": b1}))
	require.True(t, h.anchor.Equal(b1), "the earliest begin anchors the run's clock")

	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestNew", 0, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestNew", 0, time.Now()))
	require.Equal(t, []string{
		"begin pkg::TestA 2026-10-06T09:01:00Z", "end pkg::TestA",
		"begin pkg::TestNew 0001-01-01T00:00:00Z", "end pkg::TestNew",
	}, h.moves)
}

// A clock that cannot be moved fails the begin and the end: the test would
// otherwise run on another clock than its recording's.
func TestAClockThatCannotMoveFailsTheTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &clockHooks{err: errors.New("disk full")}
	useHooks(t, h)
	a := newReplayAgent()
	ctx := context.Background()
	require.ErrorContains(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()), "disk full")
	require.ErrorContains(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()), "disk full")
}

// Without the switch, and in a record, nothing moves the clock.
func TestTheClockStaysWithoutTheSwitchAndInRecord(t *testing.T) {
	h := &clockHooks{}
	useHooks(t, h)
	ctx := context.Background()

	t.Setenv(provenswitch.Env, "")
	a := newReplayAgent()
	require.NoError(t, a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))

	t.Setenv(provenswitch.Env, "1")
	r := newRecordAgent()
	require.NoError(t, r.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.NoError(t, r.EndScopeAt(ctx, "pkg::TestA", 0, time.Now()))
	require.Empty(t, h.moves)
	require.Equal(t, models.MODE_TEST, a.config.Agent.Mode)
}

// TimeFile is the build's clock record, and "" for a build that does not
// freeze the run.
func TestTimeFileIsTheBuildsClockRecord(t *testing.T) {
	useHooks(t, &AgentHook{})
	require.Equal(t, "", TimeFile())
	useHooks(t, &clockHooks{file: "/tmp/kclk1/t"})
	require.Equal(t, "/tmp/kclk1/t", TimeFile())
}
