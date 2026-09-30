package kernel

import (
	"bytes"
	"strings"
)

// Reasoning events are protocol progress, never assistant text or executable tools.
// Keep their output indexes and reconcile against the final response before tools commit.
type streamedReasoning struct {
	id    string
	done  bool
	final map[string]any
	parts map[int]*streamedPart
}

func (d *streamDelivery) consumeReasoning(v map[string]any) error {
	if !d.committed || d.meta == nil {
		return streamEventError()
	}
	index, err := streamIndex(v["output_index"])
	if err != nil {
		return err
	}
	kind := stringValue(v["type"])
	r := d.reasoning[index]
	if kind == "response.output_item.added" {
		item := objectValue(v["item"])
		id := stringValue(item["id"])
		if r != nil || d.messages[index] != nil || id == "" {
			return streamEventError()
		}
		r = &streamedReasoning{id: id, parts: map[int]*streamedPart{}}
		d.reasoning[index] = r
		return d.emit(v)
	}
	if r == nil || r.done {
		return streamEventError()
	}
	if kind == "response.output_item.done" {
		item := objectValue(v["item"])
		if err := r.validate(item); err != nil {
			return err
		}
		r.done, r.final = true, cloneObject(item)
		return d.emit(v)
	}
	if v["item_id"] != r.id {
		return streamEventError()
	}
	si, err := streamIndex(v["summary_index"])
	if err != nil {
		return err
	}
	p := r.parts[si]
	if kind == "response.reasoning_summary_part.added" {
		part := objectValue(v["part"])
		if p != nil || part["type"] != "summary_text" {
			return streamEventError()
		}
		text, ok := part["text"].(string)
		if !ok {
			return streamEventError()
		}
		p = &streamedPart{kind: "summary_text"}
		p.text.WriteString(text)
		r.parts[si] = p
	} else {
		if p == nil || p.done {
			return streamEventError()
		}
		switch kind {
		case "response.reasoning_summary_text.delta":
			text, ok := v["delta"].(string)
			if !ok || p.textDone {
				return streamEventError()
			}
			p.text.WriteString(text)
		case "response.reasoning_summary_text.done":
			if p.textDone || v["text"] != p.text.String() {
				return streamEventError()
			}
			p.textDone = true
		case "response.reasoning_summary_part.done":
			part := objectValue(v["part"])
			if part["type"] != "summary_text" || part["text"] != p.text.String() {
				return streamEventError()
			}
			p.done = true
		default:
			return streamEventError()
		}
	}
	return d.emit(v)
}

func (r *streamedReasoning) validate(item map[string]any) error {
	if item["id"] != r.id || item["type"] != "reasoning" {
		return streamEventError()
	}
	// encrypted_content is opaque and may be re-sealed between item.done and
	// response.completed. Compare the remaining fields without rewriting either
	// upstream event; preserve the terminal ciphertext in response.completed.
	previous, terminal := cloneObject(r.final), cloneObject(item)
	delete(previous, "encrypted_content")
	delete(terminal, "encrypted_content")
	if r.done && !bytes.Equal(jsonBytes(previous), jsonBytes(terminal)) {
		return streamEventError()
	}
	summary, _ := item["summary"].([]any)
	for index, part := range r.parts {
		if index >= len(summary) {
			return streamEventError()
		}
		final := objectValue(summary[index])
		text, ok := final["text"].(string)
		if !ok || final["type"] != "summary_text" || !strings.HasPrefix(text, part.text.String()) || ((part.done || part.textDone) && text != part.text.String()) {
			return streamEventError()
		}
	}
	return nil
}

func (d *streamDelivery) validateReasoning(output []any) error {
	for index, r := range d.reasoning {
		if index >= len(output) {
			return streamEventError()
		}
		if err := r.validate(objectValue(output[index])); err != nil {
			return err
		}
	}
	return nil
}
