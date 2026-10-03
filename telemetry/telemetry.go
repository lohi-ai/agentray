// Package telemetry ports Pi's explicit-parent, callback-scoped telemetry to
// Go. The zero Context is inert; NewInMemory records detached span snapshots.
// It has no dependency on a JavaScript runtime or on the application.
package telemetry

import "runtime"

// Attributes accepts strings, numbers, booleans, and slices of those values.
// A nil value represents an omitted attribute, including during a merge.
// Numeric Go types share one category; []any may mix numeric widths. JSON uses
// binary64 numbers (including integer rounding), nonfinite values become null,
// and byte slices remain numeric arrays. In-memory snapshots keep Go types.
// Callers must not mutate maps or slices concurrently with a recording call.
type Attributes map[string]any

type SpanOptions struct {
	Name       string     `json:"name"`
	Attributes Attributes `json:"attributes,omitempty"`
}

type ErrorDetails struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Error preserves an explicitly named failure when adapting serialized Pi
// errors. Ordinary Go errors continue to use the default name "Error".
func (e *ErrorDetails) Error() string { return e.Message }

type SpanStatus struct {
	Status string        `json:"status"`
	Error  *ErrorDetails `json:"error,omitempty"`
}

// Context identifies an explicit parent. Copies share the same recording scope;
// there is no global recorder or implicit goroutine-local parent.
type Context struct {
	state  *memoryState
	parent *mutableSpan
	start  func(SpanOptions, func(*Span) error) error
}

// NewContext binds a backend's synchronous callback-scoped span operation.
// The backend owns settlement, passivity, and explicit child parentage. A nil
// function gives the same no-op behavior as the zero Context.
func NewContext(start func(SpanOptions, func(*Span) error) error) Context {
	return Context{start: start}
}

// SpanCallbacks are the mutation operations supplied by a telemetry backend.
// Nil functions are inert. Backends must ignore mutations after settlement
// and suppress recording failures without changing the user's callback result.
type SpanCallbacks struct {
	AddEvent      func(string, Attributes)
	SetAttributes func(Attributes)
	SetStatus     func(SpanStatus)
}

// Span is valid as a recording parent until its callback returns. Retaining it
// is safe: later mutations are inert, and later child callbacks still execute.
type Span struct {
	context   Context
	callbacks SpanCallbacks
}

// NewSpan constructs a backend span with its explicit child context. It does
// not add lifecycle behavior; that belongs to the backend's start function.
func NewSpan(children Context, callbacks SpanCallbacks) *Span {
	return &Span{context: children, callbacks: callbacks}
}

var noopSpan = &Span{}

// Context returns this span's explicit child context.
func (s *Span) Context() Context { return s.context }

func (s *Span) StartSpan(options SpanOptions, callback func(*Span) error) error {
	return s.context.StartSpan(options, callback)
}

// StartSpan invokes callback exactly once in the calling goroutine. Returning
// an error or panicking settles the span without changing the error/panic value.
// Child work that should belong to this span must start before callback returns.
func (c Context) StartSpan(options SpanOptions, callback func(*Span) error) (err error) {
	if c.start != nil {
		return c.start(options, callback)
	}
	span := c.admit(options)
	if span == noopSpan {
		return callback(span)
	}
	returned := false
	defer func() {
		if !returned {
			value := recover()
			failure := value
			if _, nilPanic := value.(*runtime.PanicNilError); nilPanic {
				// Go wraps panic(nil), but Pi's thrown null is not an Error.
				// Normalize recording only; preserve the recovered panic below.
				failure = nil
			}
			span.settle(true, failure)
			if value != nil {
				panic(value)
			}
			return // runtime.Goexit still runs defers without becoming a panic.
		}
		span.settle(err != nil, err)
	}()
	err = callback(span)
	returned = true
	return err
}

// StartSpan preserves a typed callback result as well as its error. The method
// on Context is the equivalent for callbacks that only return an error.
func StartSpan[T any](c Context, options SpanOptions, callback func(*Span) (T, error)) (value T, err error) {
	err = c.StartSpan(options, func(span *Span) error {
		var callbackErr error
		value, callbackErr = callback(span)
		return callbackErr
	})
	return value, err
}
