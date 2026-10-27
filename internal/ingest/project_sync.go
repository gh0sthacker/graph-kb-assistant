package ingest

import (
	"context"
	"errors"
	"time"

	"bb-graph-assistant/internal/caido"
	"bb-graph-assistant/internal/project"
)

type ProjectInventorySyncOptions struct {
	DataRoot             string
	InstanceID           string
	PollInterval         time.Duration
	MissingConfirmations int
	// MaxCycles limits an operator-requested bounded inventory check. Zero
	// preserves the long-running service behavior.
	MaxCycles       int
	OnProject       func(context.Context, project.CaidoProjectRef) error
	OnProjectAbsent func(context.Context, project.CaidoProjectRef) error
	OnError         func(event string, ref *project.CaidoProjectRef, err error)
}

// RunProjectInventorySync synchronizes projects currently visible in one
// Caido instance. Inventory failures never count as omissions. A confirmed
// omission is reported to OnProjectAbsent, but this layer never deletes the
// local project, graph, or evidence; removal is a separate explicit action.
func RunProjectInventorySync(ctx context.Context, source caido.ReadSource, options ProjectInventorySyncOptions) error {
	if ctx == nil || source == nil || options.InstanceID == "" ||
		options.PollInterval < time.Second || options.PollInterval > 10*time.Minute ||
		options.MissingConfirmations < 2 || options.MissingConfirmations > 20 ||
		options.MaxCycles < 0 ||
		options.OnProject == nil || options.OnProjectAbsent == nil {
		return errors.New("Caido project inventory sync options are invalid")
	}
	for cycles := 0; ; cycles++ {
		if err := ctx.Err(); err != nil {
			return nil
		}
		var cycleErr error
		inventory, err := source.ListProjects()
		if err != nil {
			cycleErr = errors.New("Caido project inventory failed")
			if options.OnError != nil {
				options.OnError("inventory_failed", nil, err)
			}
		} else {
			missing, observeErr := project.ObserveProjectInventory(ctx, options.DataRoot, options.InstanceID, inventory, options.MissingConfirmations)
			if observeErr != nil {
				cycleErr = errors.New("Caido project inventory was rejected")
				if options.OnError != nil {
					options.OnError("inventory_rejected", nil, observeErr)
				}
			} else {
				for _, ref := range inventory {
					if err := ctx.Err(); err != nil {
						return nil
					}
					if err := options.OnProject(ctx, ref); err != nil {
						cycleErr = errors.New("one or more Caido projects failed to synchronize")
						if options.OnError != nil {
							copyRef := ref
							options.OnError("project_sync_failed", &copyRef, err)
						}
					}
				}
				for _, ref := range missing {
					if err := ctx.Err(); err != nil {
						return nil
					}
					if err := options.OnProjectAbsent(ctx, ref); err != nil {
						cycleErr = errors.New("one or more Caido project absence events failed")
						if options.OnError != nil {
							copyRef := ref
							options.OnError("project_absence_report_failed", &copyRef, err)
						}
					}
				}
			}
		}
		if options.MaxCycles > 0 && cycles+1 >= options.MaxCycles {
			return cycleErr
		}
		timer := time.NewTimer(options.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}
