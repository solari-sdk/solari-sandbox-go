package solari

import (
	"context"
	"time"
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

	raw, err := c.sb.ch.call(ctx, "code.run", params, callOptions{})
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

	// Stream text items, then flatten every present chart to the top level.
	charts := []Chart{}
	for i := range r.Results {
		item := r.Results[i]
		if item.Type == "stdout" && item.Text != "" && opts.OnStdout != nil {
			opts.OnStdout(item.Text)
		}
		if item.Type == "stderr" && item.Text != "" && opts.OnStderr != nil {
			opts.OnStderr(item.Text)
		}
		if item.Chart != nil {
			charts = append(charts, *item.Chart)
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
