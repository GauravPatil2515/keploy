package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type beginsSvc struct {
	agent.Service
	start  time.Time
	begins map[string]time.Time
}

func (s *beginsSvc) SetTestBegins(_ context.Context, start time.Time, begins map[string]time.Time) error {
	s.start, s.begins = start, begins
	return nil
}

// /agent/scope/clock hands the service the tests' recorded begins; a service
// that cannot take them answers 501, and a body it cannot read 400.
func TestHandleTestBegins(t *testing.T) {
	svc := &beginsSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	send := func(a *Agent, body string) int {
		rec := httptest.NewRecorder()
		a.HandleTestBegins(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/clock", strings.NewReader(body)))
		return rec.Code
	}
	if code := send(a, `{"start":"2026-10-06T09:00:00Z","begins":{"pkg::TestA":"2026-10-06T09:01:00Z"}}`); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	want := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	if !svc.begins["pkg::TestA"].Equal(want) || !svc.start.Equal(want.Add(-time.Minute)) {
		t.Fatalf("the service got start %v, begins %v", svc.start, svc.begins)
	}
	if code := send(a, `{`); code != http.StatusBadRequest {
		t.Fatalf("an unreadable body: status %d", code)
	}
	if code := send(&Agent{logger: zap.NewNop(), svc: &plainScopeSvc{}}, `{"begins":{}}`); code != http.StatusNotImplemented {
		t.Fatalf("a service without the capability: status %d", code)
	}
}
