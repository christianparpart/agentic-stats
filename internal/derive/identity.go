package derive

import "encoding/json"

// Identity is the format-aware material pulled out of a raw line so that the
// store can key and query it without unsealing anything.
//
// Extracting at write time is a deliberate change from the previous design,
// where ingest was entirely incurious and every query re-parsed the raw text.
// Sealed payloads make that impossible: SQL cannot read inside a ciphertext.
// The sealed line stays authoritative, and every field here is recomputable
// from it, which is what `reprocess` does.
type Identity struct {
	// SessionID and LineUUID give a line its node-independent identity.
	SessionID string
	LineUUID  string
	// CapturedAt is the line's own timestamp, not the time we received it.
	CapturedAt string
	// RequestID is the fold key. Empty on everything but assistant lines.
	RequestID string
	// Model is empty unless this line carries usage.
	Model string
	// Usage is the token accounting for the request this line belongs to.
	// Every line of a multi-block response repeats it verbatim, which is
	// exactly why the fold exists.
	Usage Usage
}

// Usage is one request's token counts.
type Usage struct {
	Input        int64
	Output       int64
	Thinking     int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// identityShape is the subset of the transcript envelope we read. Unknown
// fields are ignored, so a format that grows new ones still parses.
type identityShape struct {
	SessionID string `json:"sessionId"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Type      string `json:"type"`
	Message   struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			CacheReadInput      int64 `json:"cache_read_input_tokens"`
			OutputTokensDetails struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
			CacheCreation struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// Identify extracts identity material from a raw line.
//
// A line that is not JSON, or carries none of these fields, yields a zero
// Identity rather than an error: it is still archived, and the store falls
// back to a content-addressed key for it.
func Identify(raw []byte) Identity {
	var s identityShape
	if err := json.Unmarshal(raw, &s); err != nil {
		return Identity{}
	}
	id := Identity{
		SessionID:  s.SessionID,
		LineUUID:   s.UUID,
		CapturedAt: s.Timestamp,
		RequestID:  s.RequestID,
	}
	// Synthetic error lines carry all-zero usage and no usable request id;
	// excluding them here keeps them out of every cost calculation.
	usable := s.Type == "assistant" &&
		s.Message.Model != "" && s.Message.Model != "<synthetic>" &&
		s.Message.Usage != nil && s.RequestID != ""
	if !usable {
		// Not a billable response: keep the line, but give it no request id so
		// it cannot reach any cost calculation.
		id.RequestID = ""
		return id
	}
	u := s.Message.Usage
	id.Model = s.Message.Model
	id.Usage = Usage{
		Input:        u.InputTokens,
		Output:       u.OutputTokens,
		Thinking:     u.OutputTokensDetails.ThinkingTokens,
		CacheRead:    u.CacheReadInput,
		CacheWrite5m: u.CacheCreation.Ephemeral5m,
		CacheWrite1h: u.CacheCreation.Ephemeral1h,
	}
	return id
}
