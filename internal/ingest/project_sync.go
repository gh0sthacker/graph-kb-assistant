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
	OnProject            func(context.Context, project.CaidoProjectRef) error
	OnProjectMissing     func(context.Context, project.CaidoProjectRef) error
	OnError              func(event string, ref *project.CaidoProjectRef, err error)
}

// RunProjectInventorySync keeps every project in one Caido instance aligned
// with the local project catalog. Inventory failures never count as omissions;
// deletion callbacks run only after repeated complete successful inventories.
func RunProjectInventorySync(ctx context.Context, source caido.ReadSource, options ProjectInventorySyncOptions) error {
	if ctx == nil || source == nil || options.InstanceID == "" ||
		options.PollInterval < time.Second || options.PollInterval > 10*time.Minute ||
		options.MissingConfirmations < 2 || options.MissingConfirmations > 20 ||
		options.OnProject == nil || options.OnProjectMissing == nil {
		return errors.New("Caido project inventory sync options are invalid")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		inventory, err := source.ListProjects()
		if err != nil {
			if options.OnError != nil {
				options.OnError("inventory_failed", nil, err)
			}
		} else {
			missing, observeErr := project.ObserveProjectInventory(ctx, options.DataRoot, options.InstanceID, inventory, options.MissingConfirmations)
			if observeErr != nil {
				if options.OnError != nil {
					options.OnError("inventory_rejected", nil, observeErr)
				}
			} else {
				for _, ref := range inventory {
					if err := ctx.Err(); err != nil {
						return nil
					}
					if err := options.OnProject(ctx, ref); err != nil && options.OnError != nil {
						copyRef := ref
						options.OnError("project_sync_failed", &copyRef, err)
					}
				}
				for _, ref := range missing {
					if err := ctx.Err(); err != nil {
						return nil
					}
					if err := options.OnProjectMissing(ctx, ref); err != nil {
						if options.OnError != nil {
							copyRef := ref
							options.OnError("project_purge_failed", &copyRef, err)
						}
					}
				}
			}
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
