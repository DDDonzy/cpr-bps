package main

import (
	"bytes"
	"context"
	"cpr-excel-adapter/internal/bps"
	"cpr-excel-adapter/internal/kernel"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

type command struct {
	Op      string          `json:"op"`
	ID      string          `json:"id"`
	Scope   string          `json:"scope"`
	Request json.RawMessage `json:"request"`
	Data    string          `json:"data"`
	FileID  string          `json:"file_id"`
}
type worker struct {
	mu      sync.Mutex
	out     sync.Mutex
	jobs    map[string]*job
	history *kernel.HistoryCache
}
type job struct {
	owner     *worker
	id, scope string
	ctx       context.Context
	cancel    context.CancelFunc
	input     chan command
}

func (w *worker) emit(id, kind string, v map[string]any) {
	if v == nil {
		v = map[string]any{}
	}
	v["id"] = id
	v["type"] = kind
	w.out.Lock()
	defer w.out.Unlock()
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		os.Exit(2)
	}
}
func main() {
	w := &worker{jobs: map[string]*job{}, history: kernel.NewHistoryCache()}
	dec := json.NewDecoder(os.Stdin)
	w.emit("", "ready", map[string]any{"protocol": 1})
	for {
		var cmd command
		if err := dec.Decode(&cmd); err != nil {
			break
		}
		w.mu.Lock()
		j := w.jobs[cmd.ID]
		if cmd.Op == "open" {
			if j != nil {
				w.mu.Unlock()
				w.emit(cmd.ID, "error", map[string]any{"code": "duplicate_job", "status": 400})
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			j = &job{owner: w, id: cmd.ID, scope: cmd.Scope, ctx: ctx, cancel: cancel, input: make(chan command, 2)}
			w.jobs[cmd.ID] = j
			w.mu.Unlock()
			go j.run(cmd.Request)
			continue
		}
		w.mu.Unlock()
		if j == nil {
			continue
		}
		if cmd.Op == "cancel" {
			j.cancel()
			continue
		}
		select {
		case j.input <- cmd:
		case <-j.ctx.Done():
		}
	}
	w.mu.Lock()
	for _, j := range w.jobs {
		j.cancel()
	}
	w.mu.Unlock()
}
func (j *job) emit(kind string, v map[string]any) { j.owner.emit(j.id, kind, v) }
func (j *job) fail(err error) {
	status, code := 502, "conversion_failed"
	var api *kernel.APIError
	if errors.As(err, &api) {
		status, code = api.StatusCode(), api.Code()
	}
	j.emit("error", map[string]any{"status": status, "code": code, "message": err.Error()})
}
func (j *job) next() (command, error) {
	select {
	case c := <-j.input:
		return c, nil
	case <-j.ctx.Done():
		return command{}, j.ctx.Err()
	}
}
func (j *job) run(raw json.RawMessage) {
	defer func() { j.cancel(); j.owner.mu.Lock(); delete(j.owner.jobs, j.id); j.owner.mu.Unlock() }()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var source map[string]any
	if err := dec.Decode(&source); err != nil {
		j.fail(err)
		return
	}
	source, err := j.owner.history.Resolve(j.scope, source)
	if err != nil {
		j.fail(err)
		return
	}
	session, err := kernel.New(source, j.scope)
	if err != nil {
		j.fail(err)
		return
	}
	payload, err := session.Prepare()
	if err != nil {
		j.fail(err)
		return
	}
	images, uploads, err := bps.PrepareManagedImages(payload)
	if err != nil {
		j.fail(err)
		return
	}
	for _, u := range uploads {
		j.emit("upload", map[string]any{"content_type": u.ContentType, "body": base64.StdEncoding.EncodeToString(u.Body)})
		c, err := j.next()
		if err != nil {
			return
		}
		if c.Op != "uploaded" {
			j.fail(fmt.Errorf("attachment protocol mismatch"))
			return
		}
		if err := images.SetFile(u.Key, c.FileID); err != nil {
			j.fail(err)
			return
		}
	}
	if err := images.Apply(); err != nil {
		j.fail(err)
		return
	}
	payload["stream"] = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		j.fail(err)
		return
	}
	j.emit("prepared", map[string]any{"body": base64.StdEncoding.EncodeToString(encoded)})
	mapper := newMapper(source)
	session.OnCompleted(func(response map[string]any) {
		id, _ := response["id"].(string)
		output, _ := response["output"].([]any)
		j.owner.history.Remember(j.scope, id, source["input"], output)
	})
	reader := &jobReader{j: j}
	err = session.Relay(reader, func() {}, func(frame []byte) error {
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		events, e := mapper.frames(frame)
		if e != nil {
			return e
		}
		for _, v := range events {
			j.emit("frame", v)
		}
		return nil
	})
	if err != nil {
		var upstream *kernel.UpstreamEventError
		if !errors.As(err, &upstream) && j.ctx.Err() == nil {
			j.fail(err)
		}
	}
	if j.ctx.Err() == nil {
		stats := session.StreamingStatistics()
		j.emit("end", map[string]any{"ok": err == nil, "stats": stats})
	}
}

type jobReader struct {
	j       *job
	pending []byte
	eof     bool
}

func (r *jobReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		c, err := r.j.next()
		if err != nil {
			return 0, err
		}
		switch c.Op {
		case "data":
			r.pending, err = base64.StdEncoding.DecodeString(c.Data)
			if err != nil {
				return 0, err
			}
		case "eof":
			r.eof = true
		case "cancel":
			return 0, context.Canceled
		default:
			return 0, fmt.Errorf("unknown stream operation")
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
