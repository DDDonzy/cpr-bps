// Derived from CPA BPS v0.1.18 (MIT); see ../../third_party/CPA_LICENSE.
package kernel

import "strings"

type sseDecoder struct {
	dataBytes int
	buffer    strings.Builder
	data      []string
	event     string
}

func newSSEDecoder() *sseDecoder { return &sseDecoder{} }

func (d *sseDecoder) feed(chunk []byte, emit func(event, data string) error) error {
	if d.buffer.Len()+len(chunk) > 8<<20 {
		return fail(502, "event_too_large", "BPS SSE line exceeds its bound")
	}
	d.buffer.Write(chunk)
	text := d.buffer.String()
	for {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			d.buffer.Reset()
			d.buffer.WriteString(text)
			return nil
		}
		line := strings.TrimSuffix(text[:index], "\r")
		text = text[index+1:]
		if line == "" {
			if len(d.data) > 0 {
				if err := emit(d.event, strings.Join(d.data, "\n")); err != nil {
					return err
				}
			}
			d.data = nil
			d.dataBytes = 0
			d.event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			d.dataBytes += len(value)
			if d.dataBytes > 8<<20 {
				return fail(502, "event_too_large", "BPS SSE event exceeds its bound")
			}
			d.data = append(d.data, strings.TrimPrefix(value, " "))
		}
	}
}
