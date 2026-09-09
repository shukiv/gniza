package resticrun

import "time"

// WaitDelayOf reads the bound an OSExec puts on waiting for a killed
// child's output, so a test can check the default without waiting it out.
func WaitDelayOf(o *OSExec) time.Duration { return o.waitDelay() }
