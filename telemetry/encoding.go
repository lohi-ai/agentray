package telemetry

import "github.com/lohi-ai/agentray/internal/jsonjs"

// Build the snapshot's typed envelope as a shared JSON graph so metadata hooks
// see their real containing key (attributes/name/message), not a false root key.
func (e RecordedEvent) jsonValue() *Object {
	return NewObject(Property{Name: "name", Value: e.Name}, Property{Name: "attributes", Value: e.Attributes})
}

func (e RecordedEvent) MarshalJSON() ([]byte, error) { return jsonjs.MarshalValue(e.jsonValue()) }

func (s RecordedSpan) MarshalJSON() ([]byte, error) {
	var parent, events any
	if s.ParentID != nil {
		parent = *s.ParentID
	}
	if s.Events != nil {
		array := NewArray()
		for _, event := range s.Events {
			array.Append(event.jsonValue())
		}
		events = array
	}
	value := NewObject(Property{Name: "id", Value: s.ID}, Property{Name: "parentId", Value: parent},
		Property{Name: "name", Value: s.Name}, Property{Name: "attributes", Value: s.Attributes},
		Property{Name: "events", Value: events}, Property{Name: "status", Value: s.Status.jsonValue()},
		Property{Name: "settled", Value: s.Settled})
	if s.EndSequence != nil {
		value.Set("endSequence", *s.EndSequence)
	}
	return jsonjs.MarshalValue(value)
}
