package agentruntime

import (
	"bytes"
	"encoding/json"
)

const piTraceDeltaFormat = "pi-trace-delta-v1"

type piTraceDelta struct {
	Format     string          `json:"format"`
	BaseSeq    int             `json:"base_seq"`
	KeepPrefix int             `json:"keep_prefix"`
	Trace      json.RawMessage `json:"trace"`
}
type piTraceContext struct {
	session  string
	messages []json.RawMessage
}

func piTraceMessages(raw json.RawMessage) (map[string]json.RawMessage, map[string]json.RawMessage, []json.RawMessage, bool) {
	var trace, context map[string]json.RawMessage
	var messages []json.RawMessage
	if json.Unmarshal(raw, &trace) != nil || trace == nil || json.Unmarshal(trace["context"], &context) != nil || context == nil || json.Unmarshal(context["messages"], &messages) != nil || messages == nil {
		return nil, nil, nil, false
	}
	return trace, context, messages, true
}

// encodePiTrace applies a lossless prefix delta only to native context messages.
// Response, model, system prompt, tools, and original spans stay on every row.
// Legacy and native prefixes are independent: equal rendered text cannot prove
// that opaque signatures, image blocks, or extension fields stayed unchanged.
func encodePiTrace(raw json.RawMessage, previous []json.RawMessage, baseSeq int) (string, []json.RawMessage) {
	trace, context, messages, ok := piTraceMessages(raw)
	if !ok {
		return string(raw), nil
	}
	keep := 0
	if baseSeq != 0 {
		for keep < len(previous) && keep < len(messages) && bytes.Equal(previous[keep], messages[keep]) {
			keep++
		}
	}
	if keep == 0 {
		baseSeq = 0
	}
	context["messages"], _ = json.Marshal(messages[keep:])
	trace["context"], _ = json.Marshal(context)
	payload, _ := json.Marshal(trace)
	encoded, _ := json.Marshal(piTraceDelta{Format: piTraceDeltaFormat, BaseSeq: baseSeq, KeepPrefix: keep, Trace: payload})
	return string(encoded), messages
}

func decodePiTrace(raw string, session string, seq int, bases map[int]piTraceContext) json.RawMessage {
	if raw == "" || raw == "null" {
		return nil
	}
	var encoded piTraceDelta
	if json.Unmarshal([]byte(raw), &encoded) != nil || encoded.Format != piTraceDeltaFormat {
		if _, _, messages, ok := piTraceMessages([]byte(raw)); ok && seq != 0 {
			bases[seq] = piTraceContext{session, messages}
		}
		if !json.Valid([]byte(raw)) {
			return nil
		}
		return json.RawMessage(raw)
	}
	trace, context, messages, ok := piTraceMessages(encoded.Trace)
	if encoded.BaseSeq != 0 {
		base, found := bases[encoded.BaseSeq]
		if !found || base.session != session || encoded.KeepPrefix < 0 || encoded.KeepPrefix > len(base.messages) {
			ok = false
		} else {
			messages = append(append([]json.RawMessage{}, base.messages[:encoded.KeepPrefix]...), messages...)
		}
	} else if encoded.KeepPrefix != 0 {
		ok = false
	}
	if !ok {
		// Keep the actual stored record inspectable without presenting a partial
		// context as the original native request.
		unavailable, _ := json.Marshal(map[string]any{"format": "pi-trace-unavailable", "error": "missing or invalid native context base", "record": json.RawMessage(raw)})
		return unavailable
	}
	context["messages"], _ = json.Marshal(messages)
	trace["context"], _ = json.Marshal(context)
	full, _ := json.Marshal(trace)
	if seq != 0 {
		bases[seq] = piTraceContext{session, messages}
	}
	return full
}
