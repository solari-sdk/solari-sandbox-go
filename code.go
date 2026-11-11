package solari

import (
	"context"
	"encoding/base64"
	"strings"
	"time"
	"unicode/utf8"
)

// Code is the stateful-kernel namespace on a Sandbox (code.run).
type Code struct {
	sb *Sandbox
}

// Run executes code in a stateful kernel. Rich outputs come back as Results;
// OnStdout/OnStderr receive streamed text. Charts is a client-side convenience:
// every results[i].Chart present, flattened into a top-level slice.
func (c *Code) Run(ctx context.Context, code string, opts RunCodeOptions) (*RunCodeResult, error) {
	params := map[string]interface{}{"code": code}
	if opts.Language != "" {
		params["language"] = opts.Language
	}
	if opts.ContextID != "" {
		params["contextId"] = opts.ContextID
	}

	// Live output: with a callback the request carries stream:true, and a guest that streams sends v1 {id, stream, data} frames
	// before the result. What was delivered is tracked per stream so the result's (complete) text is only delivered beyond that:
	// a guest that predates streaming sends no frames, so its whole output is delivered once, from the result.
	var call callOptions
	var streamed map[string]*strings.Builder
	if opts.OnStdout != nil || opts.OnStderr != nil {
		params["stream"] = true
		streamed = map[string]*strings.Builder{"stdout": {}, "stderr": {}}
		dec := map[string]*utf8Stream{"stdout": {}, "stderr": {}}
		call.onStream = func(stream, data string) {
			cb := opts.OnStdout
			if stream == "stderr" {
				cb = opts.OnStderr
			} else if stream != "stdout" {
				return
			}
			raw, err := base64.StdEncoding.DecodeString(data)
			if err != nil || len(raw) == 0 {
				return
			}
			text := dec[stream].push(raw)
			if text == "" {
				return
			}
			streamed[stream].WriteString(text)
			if cb != nil {
				cb(text)
			}
		}
	}

	raw, err := c.sb.ch.call(ctx, "code.run", params, call)
	if err != nil {
		if ctx.Err() != nil {
			go c.interrupt(opts.ContextID) // in the background: the cancelled caller must not wait for the guest
		}
		return nil, err
	}

	var r RunCodeResult
	if err := unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Results == nil {
		r.Results = []CodeResultItem{}
	}

	// Deliver the text the frames did not (everything for an old guest, the unflushed tail for a streaming one), then flatten
	// every present chart to the top level.
	charts := []Chart{}
	var full = map[string]*strings.Builder{"stdout": {}, "stderr": {}}
	for i := range r.Results {
		item := r.Results[i]
		if (item.Type == "stdout" || item.Type == "stderr") && item.Text != "" {
			full[item.Type].WriteString(item.Text)
		}
		if item.Chart != nil {
			charts = append(charts, *item.Chart)
		}
	}
	for _, kind := range []string{"stdout", "stderr"} {
		cb := opts.OnStdout
		if kind == "stderr" {
			cb = opts.OnStderr
		}
		if cb == nil {
			continue
		}
		all, sent := full[kind].String(), ""
		if streamed != nil {
			sent = streamed[kind].String()
		}
		rest := all
		if sent != "" {
			if strings.HasPrefix(all, sent) {
				rest = all[len(sent):]
			} else {
				rest = ""
			}
		}
		if rest != "" {
			cb(rest)
		}
	}
	r.Charts = charts
	return &r, nil
}

// interrupt asks the guest to stop the code that a cancelled Run left executing (code.interrupt). Best effort: a guest image
// that predates the method answers "unknown method", and the call may fail for any other reason; none of it matters here.
func (c *Code) interrupt(contextID string) {
	params := map[string]interface{}{}
	if contextID != "" {
		params["contextId"] = contextID
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.sb.ch.call(bg, "code.interrupt", params, callOptions{})
}

// CreateContext creates a fresh stateful kernel context (code.context.create),
// returning its id for reuse across Run calls.
func (c *Code) CreateContext(ctx context.Context, language string) (string, error) {
	if language == "" {
		language = "python"
	}
	raw, err := c.sb.ch.call(ctx, "code.context.create", map[string]interface{}{"language": language}, callOptions{})
	if err != nil {
		return "", err
	}
	var res struct {
		ContextID string `json:"contextId"`
	}
	if err := unmarshal(raw, &res); err != nil {
		return "", err
	}
	return res.ContextID, nil
}

// utf8Stream decodes a byte stream arriving in chunks: a character split across two chunks is held back and completed by the
// next one; an invalid byte becomes U+FFFD (as validUTF8 does for a whole stream).
type utf8Stream struct{ tail []byte }

func (u *utf8Stream) push(b []byte) string {
	b = append(u.tail, b...)
	cut := len(b)
	for i := 1; i < utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				cut = len(b) - i
			}
			break
		}
	}
	u.tail = append([]byte(nil), b[cut:]...)
	return validUTF8(b[:cut])
}
