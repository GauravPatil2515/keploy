package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
)

type testBeginsSetter interface {
	SetTestBegins(ctx context.Context, start time.Time, begins map[string]time.Time) error
}

// HandleTestBegins installs when each test of the replayed recording began
// (models.TestBeginsReq). A service that cannot take them answers 501, so a
// replay that needs its tests on their recorded clocks fails instead of
// running them on another one.
func (a *Agent) HandleTestBegins(w http.ResponseWriter, r *http.Request) {
	var req models.TestBeginsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid scope-clock request: %v", err), http.StatusBadRequest)
		return
	}
	s, ok := a.svc.(testBeginsSetter)
	if !ok {
		http.Error(w, "this keploy agent cannot move the clock per test", http.StatusNotImplemented)
		return
	}
	if err := s.SetTestBegins(r.Context(), req.Start, req.Begins); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, map[string]string{"status": "ok"})
}
