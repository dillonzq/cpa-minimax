package plugin

import "encoding/json"

// Payload is the host's effective input, already in SourceFormat. Format is
// the requested output format (prepareExecutorCall in CPA v8.0.0).
func (r executorRequest) effectivePayload() []byte {
	if r.Payload != nil {
		return r.Payload
	}
	return r.OriginalRequest // legacy callers omitted/null Payload
}

func (r executorRequest) inputFormat() string {
	if r.SourceFormat != "" {
		return r.SourceFormat
	}
	if r.Format != "" {
		return r.Format
	}
	return "openai"
}

func (r executorRequest) outputFormat() string {
	if r.Format != "" {
		return r.Format
	}
	return r.inputFormat()
}

func jsonString(raw json.RawMessage) string { var s string; _ = json.Unmarshal(raw, &s); return s }
