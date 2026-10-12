package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// PushTestBegins posts the tests' recorded begins and the run's start to
// /scope/clock; an agent that does not take them is an error, never a silent
// success, because the tests would run on another clock.
func TestAgentClient_PushTestBegins(t *testing.T) {
	var got models.TestBeginsReq
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/scope/clock" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	ctx := context.Background()
	start := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

	if err := a.PushTestBegins(ctx, start, map[string]time.Time{"pkg::TestA": start.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if !got.Start.Equal(start) || !got.Begins["pkg::TestA"].Equal(start.Add(time.Minute)) {
		t.Fatalf("the agent got %+v", got)
	}
	for _, s := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusInternalServerError} {
		status = s
		err := a.PushTestBegins(ctx, start, nil)
		if err == nil || !strings.Contains(err.Error(), "did not take") {
			t.Fatalf("status %d: want an error, got %v", s, err)
		}
	}
}
