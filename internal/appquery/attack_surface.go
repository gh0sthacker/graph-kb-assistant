package appquery

import (
	"bb-graph-assistant/internal/hunt"
	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/store"
	"context"
	"errors"
	"strings"
)

type AttackSurfaceRequest struct {
	ProjectKey string   `json:"project_key"`
	Classes    []string `json:"classes,omitempty"`
	Limit      int      `json:"limit,omitempty"`
}

func ValidateAttackSurface(r AttackSurfaceRequest) error {
	if !projectKeyPattern.MatchString(r.ProjectKey) || len(r.Classes) > 32 {
		return errors.New("attack-surface project_key must be 64 lowercase hex characters; at most 32 classes")
	}
	_, err := (hunt.Graph{}).Query(r.Classes, r.Limit)
	return err
}
func ParseAttackClasses(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s,",")
}
func (s *Service) AttackSurface(ctx context.Context, r AttackSurfaceRequest) (hunt.Result, error) {
	if e := ValidateAttackSurface(r); e != nil {
		return hunt.Result{}, queryError(ErrorInvalidInput, e.Error(), e)
	}
	if s == nil {
		return hunt.Result{}, queryError(ErrorSurfaceUnavailable, "attack-surface service is unavailable", nil)
	}
	ref, ok := s.projectRef(r.ProjectKey)
	if !ok {
		return hunt.Result{}, queryError(ErrorProjectNotRegistered, "selected project is not registered", nil)
	}
	selected, e := store.OpenProjectReadOnly(ctx, ref, s.dataRoot)
	if e != nil {
		return hunt.Result{}, queryError(ErrorProjectUnavailable, "selected project is unavailable", e)
	}
	defer selected.Close()
	g, e := ingest.ReadHuntGraph(ctx, selected)
	if e != nil {
		return hunt.Result{}, queryError(ErrorSurfaceUnavailable, "hunt evidence is unavailable (check projection and historical vault access)", e)
	}
	return g.Query(r.Classes, r.Limit)
}
