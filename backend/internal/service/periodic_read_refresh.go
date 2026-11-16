package service

import "time"

// Slow-changing metadata keeps its normal success cadence. A failed read gets
// a short follow-up rather than waiting another full (potentially multi-day) cycle.
func runPeriodicReadRefresh(stop <-chan struct{}, interval time.Duration, initial, refresh func() bool) {
	ok := initial()
	for {
		delay := interval
		if !ok {
			delay = min(interval, 5*time.Minute)
		}
		timer := time.NewTimer(delay)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			ok = refresh()
		}
	}
}
