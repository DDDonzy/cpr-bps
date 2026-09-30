package kernel

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"
)

// HistoryCache 只保存已完成响应的协议历史，不保存令牌、代理凭据或磁盘副本。
// 超限/过期必须退回显式完整历史恢复，不能截断后伪装成有效续传。
type HistoryCache struct {
	mu      sync.Mutex
	entries map[historyKey]historyEntry
	bytes   int
	serial  uint64
	limits  historyLimits
	now     func() time.Time
}
type historyKey struct{ scope, response [32]byte }

func makeHistoryKey(scope, response string) historyKey {
	return historyKey{scope: sha256.Sum256([]byte(scope)), response: sha256.Sum256([]byte(response))}
}

type historyEntry struct {
	data    []byte
	expires time.Time
	serial  uint64
}
type historyLimits struct {
	totalBytes, entryBytes, entries int
	ttl                             time.Duration
}

func NewHistoryCache() *HistoryCache {
	return newHistoryCache(historyLimits{32 << 20, 16 << 20, 128, 10 * time.Minute}, time.Now)
}
func newHistoryCache(limits historyLimits, now func() time.Time) *HistoryCache {
	return &HistoryCache{entries: make(map[historyKey]historyEntry), limits: limits, now: now}
}
func (c *HistoryCache) remove(key historyKey) {
	if old, ok := c.entries[key]; ok {
		c.bytes -= len(old.data)
		delete(c.entries, key)
	}
}
func (c *HistoryCache) forget(scope, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(makeHistoryKey(scope, id))
}

func (c *HistoryCache) expire(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			c.remove(key)
		}
	}
}
func historyItems(value any) ([]any, bool) {
	switch v := value.(type) {
	case nil:
		return nil, true
	case []any:
		return v, true
	case string:
		return []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": v}}}}, true
	default:
		return nil, false
	}
}

// Resolve 只在缓存命中后移除 previous_response_id，并精确追加本轮 delta。
func (c *HistoryCache) Resolve(scope string, request map[string]any) (map[string]any, error) {
	previous, exists := request["previous_response_id"]
	if !exists || previous == nil {
		return cloneObject(request), nil
	}
	id, ok := previous.(string)
	if !ok || id == "" {
		return nil, fail(400, "invalid_previous_response_id", "previous_response_id must be non-empty text")
	}
	delta, ok := historyItems(request["input"])
	if !ok {
		return nil, fail(400, "invalid_input", "history entries must be an array or text")
	}
	c.mu.Lock()
	c.expire(c.now())
	entry, found := c.entries[makeHistoryKey(scope, id)]
	data := bytes.Clone(entry.data)
	c.mu.Unlock()
	if !found {
		return nil, fail(400, "previous_response_not_found", "previous response is unavailable in this scope; replay complete history")
	}
	var history []any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&history); err != nil {
		return nil, fail(500, "invalid_history_cache", "continuation history is unavailable")
	}
	result := cloneObject(request)
	result["input"] = append(history, delta...)
	delete(result, "previous_response_id")
	return result, nil
}

// Remember 的 output 必须是已验证终态中交付给客户端的 output，不能保存半截流。
func (c *HistoryCache) Remember(scope, id string, input any, output []any) bool {
	if scope == "" || id == "" {
		return false
	}
	items, ok := historyItems(input)
	if !ok {
		c.forget(scope, id)
		return false
	}
	history := make([]any, 0, len(items)+len(output))
	history = append(history, items...)
	history = append(history, output...)
	data, err := json.Marshal(history)
	if err != nil || len(data) > c.limits.entryBytes || len(data) > c.limits.totalBytes {
		c.forget(scope, id)
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.expire(now)
	key := makeHistoryKey(scope, id)
	if old, exists := c.entries[key]; exists {
		if bytes.Equal(old.data, data) {
			return true
		}
		// 上游重用 ID 时既不能覆盖，也不能继续返回旧历史；让客户端明确恢复。
		c.remove(key)
		return false
	}
	for c.bytes+len(data) > c.limits.totalBytes || len(c.entries) >= c.limits.entries {
		var oldest historyKey
		serial := ^uint64(0)
		for k, e := range c.entries {
			if e.serial < serial {
				oldest = k
				serial = e.serial
			}
		}
		if serial == ^uint64(0) {
			return false
		}
		c.remove(oldest)
	}
	c.serial++
	c.entries[key] = historyEntry{data: data, expires: now.Add(c.limits.ttl), serial: c.serial}
	c.bytes += len(data)
	return true
}
