// Derived from CPA BPS v0.1.18 (MIT); see ../../third_party/CPA_LICENSE.
package kernel

import "bytes"

func codexTerminalResponse(response map[string]any) []byte {
	event := "response.completed"
	if response["status"] == "incomplete" {
		event = "response.incomplete"
	}
	return jsonBytes(map[string]any{"type": event, "response": response})
}

func executorStreamPayloads(format string, response map[string]any) [][]byte {
	payload := syntheticStream(response)
	if format != "codex" {
		return [][]byte{payload}
	}
	// 宿主每次回调只翻译一个 data 事件；整段 SSE 会被转换器忽略。
	var frames [][]byte
	for _, line := range bytes.Split(payload, []byte{10}) {
		if bytes.HasPrefix(line, []byte("data: ")) && !bytes.Equal(line, []byte("data: [DONE]")) {
			frames = append(frames, bytes.Clone(line))
		}
	}
	return frames
}
