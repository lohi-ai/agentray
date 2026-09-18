package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/lohi-ai/agentray/agentcore"
)

const maxEvalFrameBytes = 1024 * 1024

const (
	maxEvalImagesPerCell = 8
	maxEvalImageBytes    = 768 * 1024
)

type evalFrame struct {
	Type           string                     `json:"type"`
	ID             string                     `json:"id,omitempty"`
	Data           string                     `json:"data,omitempty"`
	Status         string                     `json:"status,omitempty"`
	ExecutionCount int                        `json:"execution_count,omitempty"`
	Bundle         map[string]json.RawMessage `json:"bundle,omitempty"`
}

type evalCellRequest struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}

type evalCellResult struct {
	Output         string
	Parts          []agentcore.ContentPart
	ExecutionCount int
	RuntimeError   bool
	Truncated      bool
}

type evalProcess struct {
	proc agentcore.SandboxProcess
	in   io.WriteCloser

	writeMu sync.Mutex
	frames  chan evalFrame
	errs    chan error
	done    chan struct{}

	stderr    cappedStringWriter
	closeOnce sync.Once
	nextID    uint64
	cleanup   func()
}

func newEvalProcess(proc agentcore.SandboxProcess, cleanup func()) *evalProcess {
	if cleanup == nil {
		cleanup = func() {}
	}
	p := &evalProcess{
		proc: proc, in: proc.Stdin(), frames: make(chan evalFrame, 64),
		errs: make(chan error, 1), done: make(chan struct{}), cleanup: cleanup,
	}
	p.stderr.limit = 8 * 1024
	go p.readFrames(proc.Stdout())
	go func() { _, _ = io.Copy(&p.stderr, proc.Stderr()) }()
	go func() {
		_, _ = proc.Wait()
		p.cleanup()
		close(p.done)
	}()
	return p
}

func (p *evalProcess) readFrames(r io.Reader) {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				select {
				case p.errs <- fmt.Errorf("read eval protocol: %w", err):
				default:
				}
			}
			return
		}
		if len(line) > maxEvalFrameBytes {
			select {
			case p.errs <- fmt.Errorf("eval protocol frame exceeds %d bytes", maxEvalFrameBytes):
			default:
			}
			return
		}
		var frame evalFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			select {
			case p.errs <- fmt.Errorf("decode eval protocol: %w", err):
			default:
			}
			return
		}
		select {
		case p.frames <- frame:
		case <-p.done:
			return
		}
	}
}

func (p *evalProcess) execute(ctx context.Context, code string, limit int) (evalCellResult, error) {
	p.nextID++
	id := fmt.Sprintf("cell-%d", p.nextID)
	payload, err := json.Marshal(evalCellRequest{ID: id, Code: code})
	if err != nil {
		return evalCellResult{}, err
	}
	p.writeMu.Lock()
	_, err = p.in.Write(append(payload, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		return evalCellResult{}, withEvalStderr(fmt.Errorf("write eval request: %w", err), p.stderr.String())
	}

	output := newBoundedEvalOutput(limit)
	result := evalCellResult{}
	imageBytes := 0
	for {
		select {
		case <-ctx.Done():
			return evalCellResult{}, ctx.Err()
		case err := <-p.errs:
			return evalCellResult{}, withEvalStderr(err, p.stderr.String())
		case <-p.done:
			return evalCellResult{}, withEvalStderr(errEvalKernelClosed, p.stderr.String())
		case frame := <-p.frames:
			if frame.ID != id {
				continue
			}
			switch frame.Type {
			case "stdout":
				output.WriteSection("stdout", frame.Data)
			case "stderr":
				output.WriteSection("stderr", frame.Data)
			case "result":
				output.WriteSection("result", frame.Data)
			case "error":
				result.RuntimeError = true
				output.WriteSection("error", frame.Data)
			case "display":
				text, parts, bytes := renderEvalDisplay(frame.Bundle, maxEvalImagesPerCell-len(result.Parts), maxEvalImageBytes-imageBytes)
				kind := "display"
				if frame.Status == "result" {
					kind = "result"
				}
				output.WriteSection(kind, text)
				result.Parts = append(result.Parts, parts...)
				imageBytes += bytes
			case "done":
				result.ExecutionCount = frame.ExecutionCount
				result.Output, result.Truncated = output.String()
				return result, nil
			}
		}
	}
}

func renderEvalDisplay(bundle map[string]json.RawMessage, imageSlots, imageBudget int) (string, []agentcore.ContentPart, int) {
	var text string
	for _, mime := range []string{"text/markdown", "text/plain", "text/html", "image/svg+xml", "text/latex"} {
		raw, ok := bundle[mime]
		if !ok {
			continue
		}
		if json.Unmarshal(raw, &text) == nil && text != "" {
			if mime == "text/html" {
				text = "[HTML display]\n" + text
			} else if mime == "image/svg+xml" {
				text = "[SVG display]\n" + text
			} else if mime == "text/latex" {
				text = "[LaTeX display]\n" + text
			}
			break
		}
	}
	if raw, ok := bundle["application/json"]; ok {
		var value any
		if json.Unmarshal(raw, &value) == nil {
			if pretty, err := json.MarshalIndent(value, "", "  "); err == nil {
				text = string(pretty)
			}
		}
	}

	parts := make([]agentcore.ContentPart, 0, 2)
	used := 0
	appendNotice := func(notice string) {
		if text != "" {
			text += "\n"
		}
		text += notice
	}
	for _, mime := range []string{"image/png", "image/jpeg"} {
		raw, ok := bundle[mime]
		if !ok {
			continue
		}
		if imageSlots <= len(parts) {
			appendNotice(fmt.Sprintf("[%s display omitted: per-cell image limit reached]", mime))
			continue
		}
		if imageBudget-used <= 0 {
			appendNotice(fmt.Sprintf("[%s display omitted: cell image budget exhausted]", mime))
			continue
		}
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil || encoded == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || !validEvalImage(mime, decoded) {
			appendNotice(fmt.Sprintf("[%s display omitted: invalid image data]", mime))
			continue
		}
		if len(decoded) > imageBudget-used {
			appendNotice(fmt.Sprintf("[%s display omitted: image exceeds remaining %d-byte cell budget]", mime, imageBudget-used))
			continue
		}
		parts = append(parts, agentcore.ContentPart{
			Type: agentcore.ContentPartImage, MIMEType: mime, Data: encoded,
		})
		used += len(decoded)
	}
	if len(parts) > 0 {
		note := fmt.Sprintf("[%d rich image display(s) attached]", len(parts))
		if text == "" {
			text = note
		} else {
			text += "\n" + note
		}
	}
	return text, parts, used
}

func validEvalImage(mime string, data []byte) bool {
	switch mime {
	case "image/png":
		return bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
	default:
		return false
	}
}

func (p *evalProcess) closeProcess() {
	p.closeOnce.Do(func() {
		p.writeMu.Lock()
		_, _ = io.WriteString(p.in, "{\"type\":\"exit\"}\n")
		_ = p.in.Close()
		p.writeMu.Unlock()
		select {
		case <-p.done:
			return
		case <-time.After(500 * time.Millisecond):
			_ = p.proc.Kill()
			<-p.done
		}
	})
}

// close exists as a method value used by evalKernel without exposing the
// protocol process outside this package.
func (p *evalProcess) close() { p.closeProcess() }

type boundedEvalOutput struct {
	limit     int
	total     int
	headLimit int
	tailLimit int
	head      strings.Builder
	tail      []byte
	lastKind  string
}

func newBoundedEvalOutput(limit int) *boundedEvalOutput {
	if limit < 1 {
		limit = defaultEvalMaxOutputBytes
	}
	return &boundedEvalOutput{limit: limit, headLimit: limit * 2 / 3, tailLimit: limit - limit*2/3}
}

func (b *boundedEvalOutput) WriteSection(kind, value string) {
	if value == "" {
		return
	}
	if b.lastKind != kind {
		if b.total > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[" + kind + "]\n")
		b.lastKind = kind
	}
	b.WriteString(value)
}

func (b *boundedEvalOutput) WriteString(value string) {
	if value == "" {
		return
	}
	data := []byte(value)
	b.total += len(data)
	if b.head.Len() < b.headLimit {
		n := b.headLimit - b.head.Len()
		if n > len(data) {
			n = len(data)
		}
		_, _ = b.head.Write(data[:n])
		data = data[n:]
	}
	if len(data) == 0 || b.tailLimit == 0 {
		return
	}
	if len(data) >= b.tailLimit {
		b.tail = append(b.tail[:0], data[len(data)-b.tailLimit:]...)
		return
	}
	if len(b.tail)+len(data) > b.tailLimit {
		drop := len(b.tail) + len(data) - b.tailLimit
		copy(b.tail, b.tail[drop:])
		b.tail = b.tail[:len(b.tail)-drop]
	}
	b.tail = append(b.tail, data...)
}

func (b *boundedEvalOutput) String() (string, bool) {
	if b.total <= b.limit {
		return b.head.String() + string(b.tail), false
	}
	omitted := b.total - b.head.Len() - len(b.tail)
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", b.head.String(), omitted, b.tail), true
}

func withEvalStderr(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w; kernel stderr: %s", err, stderr)
}
