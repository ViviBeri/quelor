package decider

import "os"

// EnvDecider is the process kill switch. QUELOR_DECIDER=static forces the
// reference Decider.
const EnvDecider = "QUELOR_DECIDER"

// ForceStatic reports whether EnvDecider is exactly "static".
func ForceStatic() bool {
	return os.Getenv(EnvDecider) == "static"
}

// Select returns reference when the kill switch is on or configured is nil.
// Otherwise it returns configured.
func Select(configured, reference Decider) Decider {
	if ForceStatic() || configured == nil {
		return reference
	}
	return configured
}
