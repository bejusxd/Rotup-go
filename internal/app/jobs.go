package app

import "time"

func (a *Application) planJobs() {
	defer a.background.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.workCtx.Done():
			return
		case <-ticker.C:
			if _, err := a.planner.Tick(a.workCtx); err != nil && a.workCtx.Err() == nil {
				a.logger.Error("job planning failed", "error", err)
			}
			if err := a.store.ReconcileJobChains(a.workCtx, time.Now().UTC()); err != nil && a.workCtx.Err() == nil {
				a.logger.Error("job chaining failed", "error", err)
			}
		}
	}
}

func (a *Application) dispatchJobs() {
	defer a.background.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.workCtx.Done():
			return
		case <-ticker.C:
			if err := a.dispatcher.Tick(a.workCtx); err != nil && a.workCtx.Err() == nil {
				a.logger.Error("job dispatch failed", "error", err)
			}
		}
	}
}
