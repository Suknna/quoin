// Package clock supplies the production timer used by inspection scheduling.
package clock

import "time"

// System reads and waits on the host clock. Tests inject the scheduler's
// package-private clock interface instead of changing process time.
type System struct{}

func (System) Now() time.Time { return time.Now() }
func (System) After(wait time.Duration) <-chan time.Time {
	return time.After(wait)
}
