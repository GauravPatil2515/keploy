package mock

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A test keeps the begin of its window in the recording; a test begun twice
// keeps the first, and a window without a start gives none.
func TestStampBeginsKeepsEachTestsFirstBegin(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	byCase := map[string]models.MappedTestCase{"pkg::TestA": {Dir: "pkg"}}
	stampBegins(byCase, []models.ScopeWindow{
		{Name: "pkg::TestA", Start: t0.Add(time.Minute), End: t0.Add(2 * time.Minute)},
		{Name: "pkg::TestA", Start: t0, End: t0.Add(time.Second)},
		{Name: "pkg::TestB", Start: t0.Add(time.Hour)},
		{Name: "pkg::TestC"},
	})
	if got := byCase["pkg::TestA"]; !got.At.Equal(t0) || got.Dir != "pkg" {
		t.Fatalf("pkg::TestA: %+v", got)
	}
	if got := byCase["pkg::TestB"]; !got.At.Equal(t0.Add(time.Hour)) {
		t.Fatalf("pkg::TestB: %+v", got)
	}
	if got, ok := byCase["pkg::TestC"]; ok {
		t.Fatalf("a window with no start gave pkg::TestC %+v", got)
	}
}

type beginsPusher struct {
	Instrumentation
	start  time.Time
	begins map[string]time.Time
	calls  int
}

func (p *beginsPusher) PushTestBegins(_ context.Context, start time.Time, begins map[string]time.Time) error {
	p.start, p.begins = start, begins
	p.calls++
	return nil
}

// Replay hands the agent each test's recorded begin and the earliest of them;
// a recording without begins still sends an (empty) table, so an agent reused
// across sessions forgets the previous one.
func TestPushTestBeginsSendsTheRecordingsBegins(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	p := &beginsPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{
			{ID: "pkg::TestA", At: t0.Add(time.Minute)},
			{ID: "pkg::TestB", At: t0},
			{ID: "pkg::TestOld"},
		},
	}}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if !p.start.Equal(t0) || len(p.begins) != 2 || !p.begins["pkg::TestA"].Equal(t0.Add(time.Minute)) {
		t.Fatalf("pushed start %v, begins %v", p.start, p.begins)
	}

	p = &beginsPusher{}
	m = &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{}}
	if err := m.pushTestBegins(context.Background(), "rec"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || !p.start.IsZero() || len(p.begins) != 0 {
		t.Fatalf("pushed start %v, begins %v (%d calls)", p.start, p.begins, p.calls)
	}
}

// An agent client that cannot move the clock per test refuses the replay.
func TestPushTestBeginsRefusesWithoutTheCapability(t *testing.T) {
	m := &mockService{logger: zap.NewNop(), instrumentation: &setTablePusher{}, mappingDB: setTableDB{}}
	if err := m.pushTestBegins(context.Background(), "rec"); err == nil {
		t.Fatal("want an error")
	}
}
