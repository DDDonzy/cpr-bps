package kernel

// Codex 的 sequential_cutoff 控制摘要交付顺序，不是模型生成参数。
// 当前桥只流出已验证的正文，推理项留在完整终态内，不流出并发摘要增量；
// 因而无需向 BPS 透传该控制项，也不裁剪最终响应或加密推理历史。
func validateStreamOptions(value any) error {
	if value == nil {
		return nil
	}
	options, ok := value.(map[string]any)
	if !ok {
		return fail(400, "invalid_stream_options", "stream_options must be an object")
	}
	for name, value := range options {
		switch name {
		case "reasoning_summary_delivery":
			if value != "sequential_cutoff" {
				return fail(400, "unsupported_stream_options", "reasoning summary delivery mode is unsupported")
			}
		default:
			return fail(400, "unsupported_stream_options", "stream option is unsupported")
		}
	}
	return nil
}
