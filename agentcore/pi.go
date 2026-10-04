package agentcore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// PiCallback serves an upstream provider, tool, or optional hook. Params and
// results use Pi's native JSON vocabulary without Go Message conversions.
// For stream callbacks, emit accepts AssistantMessageEvent and the return value
// is the final AssistantMessage. For tools, emit accepts AgentToolResult updates.
// Native onPayload receives {payload, model}; return nil to preserve the payload.
// Callbacks may run concurrently and must honor ctx cancellation.
type PiCallback func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error)

// PiConfig configures an isolated process running the unchanged upstream Agent.
// Build Worker with `bun run build` in third_party/pi. It is dist/worker.mjs;
// keep the other generated files beside it. Runtime defaults to "bun"; hosts
// may explicitly select "node" (22.19+) instead.
type PiConfig struct {
	Runtime string
	Worker  string
	// Options carries native AgentOptions, callback names in a callbacks array,
	// and streamMode ("callback", the default, or "native" for Pi's provider APIs).
	Options  json.RawMessage
	Callback PiCallback
	// OnEvent is awaited by Pi before it proceeds. It may call State, Steer,
	// FollowUp or Abort, but must not wait for the run's own completion.
	OnEvent func(context.Context, json.RawMessage) error
	// OnTrace receives an original native request/response and settled Pi spans.
	// It is observational: panics are isolated and no result controls the loop.
	OnTrace func(context.Context, json.RawMessage)
	Stderr  io.Writer
}

// PiError is an error returned by the upstream runtime or a host callback.
type PiError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

func (e *PiError) Error() string { return e.Name + ": " + e.Message }

type piFrame struct {
	Kind   string          `json:"kind"`
	ID     string          `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
	Error  *PiError        `json:"error,omitempty"`
}

// PiAgent runs Pi itself across an explicit JSON process boundary. The reference
// TypeScript bundles live under third_party/pi; this bridge is transitional. Unlike
// Agent, it adds no retry, compaction, session, permission, or budget policy.
// Such policies must be supplied through Pi's host callbacks.
type PiAgent struct {
	cmd      *exec.Cmd
	input    io.WriteCloser
	cancel   context.CancelFunc
	ctx      context.Context
	config   PiConfig
	writeMu  sync.Mutex
	mu       sync.Mutex
	pending  map[string]chan piFrame
	handlers map[string]context.CancelFunc
	nextID   atomic.Uint64
	done     chan struct{}
	err      error
	commit   string
}

// NewPi starts the pinned TypeScript runtime. ctx owns the process lifetime;
// cancel it or call Close to release the process and cancel active callbacks.
func NewPi(ctx context.Context, cfg PiConfig) (*PiAgent, error) {
	if cfg.Worker == "" {
		return nil, errors.New("agentcore: Pi worker path is required")
	}
	if cfg.Runtime == "" {
		cfg.Runtime = "bun"
	}
	life, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(life, cfg.Runtime, cfg.Worker)
	cmd.Stderr = cfg.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = input.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		return nil, fmt.Errorf("agentcore: start Pi: %w", err)
	}
	a := &PiAgent{cmd: cmd, input: input, cancel: cancel, ctx: life, config: cfg,
		pending: make(map[string]chan piFrame), handlers: make(map[string]context.CancelFunc), done: make(chan struct{})}
	go a.read(output)
	options := cfg.Options
	if len(options) == 0 {
		options = json.RawMessage(`{}`)
	}
	if cfg.OnTrace != nil {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(options, &values); err != nil || values == nil {
			_ = a.Close()
			return nil, errors.New("Pi options must be an object")
		}
		values["traceRequests"] = json.RawMessage(`true`)
		options, _ = json.Marshal(values)
	}
	result, err := a.Call(ctx, "initialize", options)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	var handshake struct {
		Protocol int    `json:"protocol"`
		Commit   string `json:"upstreamCommit"`
	}
	if err := json.Unmarshal(result, &handshake); err != nil || handshake.Protocol != 1 || handshake.Commit == "" {
		_ = a.Close()
		return nil, errors.New("agentcore: incompatible Pi worker handshake")
	}
	a.commit = handshake.Commit
	return a, nil
}

// UpstreamCommit identifies the actual worker build used by this process.
func (a *PiAgent) UpstreamCommit() string { return a.commit }

func (a *PiAgent) write(frame piFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, err = a.input.Write(append(data, '\n'))
	return err
}

func (a *PiAgent) read(output io.ReadCloser) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	var transportErr error
	for scanner.Scan() {
		var frame piFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			transportErr = fmt.Errorf("agentcore: invalid Pi response: %w", err)
			break
		}
		switch frame.Kind {
		case "result":
			a.mu.Lock()
			ch := a.pending[frame.ID]
			delete(a.pending, frame.ID)
			a.mu.Unlock()
			if ch != nil {
				ch <- frame
			}
		case "callback":
			ctx, cancel := context.WithCancel(a.ctx)
			a.mu.Lock()
			a.handlers[frame.ID] = cancel
			a.mu.Unlock()
			go a.callback(ctx, cancel, frame)
		case "cancel":
			a.mu.Lock()
			cancel := a.handlers[frame.ID]
			a.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		default:
			transportErr = fmt.Errorf("agentcore: unknown Pi frame %q", frame.Kind)
		}
		if transportErr != nil {
			break
		}
	}
	if transportErr == nil {
		transportErr = scanner.Err()
	}
	a.cancel()
	_ = a.input.Close()
	_ = output.Close()
	if err := a.cmd.Wait(); transportErr == nil {
		transportErr = err
	}
	if transportErr == nil {
		transportErr = io.EOF
	}
	a.mu.Lock()
	a.err = transportErr
	a.pending = make(map[string]chan piFrame)
	for _, cancel := range a.handlers {
		cancel()
	}
	a.mu.Unlock()
	close(a.done)
}

func (a *PiAgent) callback(ctx context.Context, cancel context.CancelFunc, frame piFrame) {
	defer cancel()
	defer func() {
		a.mu.Lock()
		delete(a.handlers, frame.ID)
		a.mu.Unlock()
	}()
	var progressMu sync.Mutex
	settled := false
	emit := func(value json.RawMessage) error {
		progressMu.Lock()
		defer progressMu.Unlock()
		if settled || ctx.Err() != nil {
			return context.Canceled
		}
		return a.write(piFrame{Kind: "progress", ID: frame.ID, Value: value})
	}
	value, err := func() (value json.RawMessage, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("Pi callback panic: %v", recovered)
			}
		}()
		if frame.Method == "event" {
			if a.config.OnEvent != nil {
				err = a.config.OnEvent(ctx, frame.Params)
			}
			return nil, err
		}
		if frame.Method == "trace" {
			func() {
				defer func() { _ = recover() }()
				if a.config.OnTrace != nil {
					a.config.OnTrace(ctx, frame.Params)
				}
			}()
			return nil, nil
		}
		if a.config.Callback == nil {
			return nil, fmt.Errorf("no Pi callback handler for %s", frame.Method)
		}
		return a.config.Callback(ctx, frame.Method, frame.Params, emit)
	}()
	progressMu.Lock()
	defer progressMu.Unlock()
	settled = true
	if ctx.Err() != nil {
		return
	}
	result := piFrame{Kind: "result", ID: frame.ID, Value: value}
	if err == nil && len(value) > 0 && !json.Valid(value) {
		err = errors.New("Pi callback returned invalid JSON")
	}
	if err != nil {
		result.Value = nil
		result.Error = &PiError{Name: "Error", Message: err.Error()}
		var piErr *PiError
		if errors.As(err, &piErr) {
			result.Error = piErr
		}
	}
	if err := a.write(result); err != nil {
		a.cancel()
	}
}

// Call invokes a public Pi operation. The JSON result retains upstream names
// and values; pendingToolCalls is represented as an array instead of a JS Set.
// Cancelling prompt/continue aborts only that invocation, never a later run.
func (a *PiAgent) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("call-%d", a.nextID.Add(1))
	ch := make(chan piFrame, 1)
	a.mu.Lock()
	if a.err != nil {
		err := a.err
		a.mu.Unlock()
		return nil, err
	}
	a.pending[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()
	if err := a.write(piFrame{Kind: "call", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case frame := <-ch:
		if frame.Error != nil {
			return nil, frame.Error
		}
		return frame.Value, nil
	case <-ctx.Done():
		if method == "prompt" || method == "continue" {
			_ = a.write(piFrame{Kind: "cancelCall", ID: id})
		}
		return nil, ctx.Err()
	case <-a.done:
		a.mu.Lock()
		err := a.err
		a.mu.Unlock()
		return nil, err
	}
}

// Prompt accepts a Pi message, an array of Pi messages, or a JSON string.
func (a *PiAgent) Prompt(ctx context.Context, input json.RawMessage) error {
	params, err := json.Marshal(struct {
		Input json.RawMessage `json:"input"`
	}{Input: input})
	if err != nil {
		return err
	}
	_, err = a.Call(ctx, "prompt", params)
	return err
}

func (a *PiAgent) Continue(ctx context.Context) error {
	_, err := a.Call(ctx, "continue", nil)
	return err
}

func (a *PiAgent) State(ctx context.Context) (json.RawMessage, error) {
	return a.Call(ctx, "state", nil)
}

// Close cancels active callbacks and reaps the worker. It is idempotent.
func (a *PiAgent) Close() error {
	a.cancel()
	<-a.done
	return nil
}
