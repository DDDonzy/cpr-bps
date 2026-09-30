package kernel

import "time"

// Statistics are published after Relay returns and contain no request contents.
type StreamStatistics struct {
	UpstreamReadChunks    int
	UpstreamBytes         int64
	UpstreamEvents        int
	LastUpstreamDataAgeMS int64
	ToolArgumentDeltas    int
	ToolArgumentBytes     int64
	ToolProgressEvents    int
}

// Report real generation of buffered tool arguments as response activity.
// The wrapper may reveal the destination tool only near its end, so its partial
// arguments cannot yet be safely translated into an executable client tool call.
// No timer manufactures activity: silence upstream remains silence downstream.
func (d *streamDelivery) toolArgumentActivity(delta string) error {
	d.stats.ToolArgumentDeltas++
	d.stats.ToolArgumentBytes += int64(len(delta))
	if !d.committed || d.meta == nil {
		return nil
	}
	now := d.clock()
	if !d.lastToolProgress.IsZero() && now.Sub(d.lastToolProgress) < 15*time.Second {
		return nil
	}
	response := cloneObject(d.meta)
	response["status"], response["output"] = "in_progress", []any{}
	if err := d.emit(map[string]any{"type": "response.in_progress", "response": response}); err != nil {
		return err
	}
	d.lastToolProgress = now
	d.stats.ToolProgressEvents++
	return nil
}

// StreamingStatistics must be read only after the request's Relay has returned.
func (s *Session) StreamingStatistics() StreamStatistics { return s.streamStats }
