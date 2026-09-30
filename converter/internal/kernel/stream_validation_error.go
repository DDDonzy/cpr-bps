package kernel

// StreamValidationError records only the source validation site, never event data.
// Unwrap preserves existing public protocol errors and upstream error passthrough.
type StreamValidationError struct {
	*APIError
	Location string
}

func (e *StreamValidationError) Unwrap() error { return e.APIError }
