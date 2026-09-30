package kernel

// UpstreamEventError keeps the original error event instead of replacing its fields.
type UpstreamEventError struct{ Event map[string]any }

func (e *UpstreamEventError) Error() string { return "BPS upstream returned a failure event" }
func upstreamEventError(value map[string]any) error {
	return &UpstreamEventError{Event: cloneObject(value)}
}
