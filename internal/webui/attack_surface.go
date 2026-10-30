package webui

import (
	"bb-graph-assistant/internal/appquery"
	"bb-graph-assistant/internal/hunt"
	"context"
	"net/http"
	"strconv"
)

type attackSurfaceAPI interface {
	AttackSurface(context.Context, appquery.AttackSurfaceRequest) (hunt.Result, error)
}

func attackSurfaceHandler(api attackSurfaceAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for k, values := range q {
			if (k != "project_key" && k != "classes" && k != "limit") || len(values) != 1 {
				writeError(w, http.StatusBadRequest, "attack-surface query is malformed")
				return
			}
		}
		req := appquery.AttackSurfaceRequest{ProjectKey: q.Get("project_key"), Classes: appquery.ParseAttackClasses(q.Get("classes")), Limit: 50}
		if v, ok := q["limit"]; ok {
			n, e := strconv.Atoi(v[0])
			if e != nil || n < 1 {
				writeError(w, http.StatusBadRequest, "limit must be between 1 and 1000")
				return
			}
			req.Limit = n
		}
		if e := appquery.ValidateAttackSurface(req); e != nil {
			writeError(w, http.StatusBadRequest, e.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), graphTimeout)
		defer cancel()
		result, e := api.AttackSurface(ctx, req)
		writeGraphResult(w, map[string]any{"candidates": result.Candidates, "uncovered": result.Uncovered, "stats": result.Stats, "coverage_gaps": result.CoverageGaps}, e)
	}
}
