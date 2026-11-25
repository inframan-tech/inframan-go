package inframan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Poll calls check at once and then every `every`, until it says done,
// returns an error, or ctx ends (ctx's error then). Use it to wait for a run,
// a release or a provisioning to finish:
//
//	err := inframan.Poll(ctx, 5*time.Second, func(ctx context.Context) (bool, error) {
//		raw, err := c.GetAgentRun(ctx, inframan.GetAgentRunParams{ServiceID: agent, RunID: run})
//		…
//		return finished, err
//	})
func Poll(ctx context.Context, every time.Duration, check func(ctx context.Context) (done bool, err error)) error {
	if every <= 0 {
		return fmt.Errorf("inframan: Poll: every is %s; it must be positive", every)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// StopPaging, returned by a Pages callback, stops paging with no error.
var StopPaging = errors.New("inframan: stop paging")

// Pages calls a paged capability page after page, handing each answer to
// page, until an answer is the last, page returns an error (StopPaging stops
// with none), or a call fails.
//
// How a capability pages is read from its parameters. One that takes a
// cursor is sent each answer's next_cursor, until an answer carries none; one
// that takes an offset is sent each answer's next_offset, until it is null or
// negative. Each is read at the top of the answer, or, where the top has
// none, in the one object at its top that carries it (get_agent_run's is the
// run's). args is the first page's; it is not changed.
func (c *Client) Pages(ctx context.Context, name string, args map[string]any, page func(json.RawMessage) error) error {
	r, ok := RouteOf(name)
	if !ok {
		return fmt.Errorf("inframan: no capability %q is known to this client", name)
	}
	_, byCursor := r.In["cursor"]
	_, byOffset := r.In["offset"]
	if !byCursor && !byOffset {
		return fmt.Errorf("inframan: %s takes neither a cursor nor an offset: it is not paged", name)
	}
	a := make(map[string]any, len(args)+1)
	for k, v := range args {
		a[k] = v
	}
	var lastCursor string
	lastOffset := int64(-1)
	if v, ok := a["offset"]; ok {
		if n, ok := asInt(v); ok {
			lastOffset = n
		}
	}
	for {
		var raw json.RawMessage
		if err := c.Call(ctx, name, a, &raw); err != nil {
			return err
		}
		if err := page(raw); err != nil {
			if errors.Is(err, StopPaging) {
				return nil
			}
			return err
		}
		if byCursor {
			var next *string
			if v, ok := nextOf(raw, "next_cursor"); ok {
				if err := json.Unmarshal(v, &next); err != nil {
					return fmt.Errorf("inframan: %s: read next_cursor: %w", name, err)
				}
			}
			if next == nil || *next == "" {
				return nil
			}
			if *next == lastCursor {
				return fmt.Errorf("inframan: %s: the answer's next_cursor is the one just sent", name)
			}
			lastCursor = *next
			a["cursor"] = lastCursor
			continue
		}
		var next *int64
		if v, ok := nextOf(raw, "next_offset"); ok {
			if err := json.Unmarshal(v, &next); err != nil {
				return fmt.Errorf("inframan: %s: read next_offset: %w", name, err)
			}
		}
		if next == nil || *next < 0 {
			return nil
		}
		if *next <= lastOffset {
			return fmt.Errorf("inframan: %s: the answer's next_offset %d does not move past %d", name, *next, lastOffset)
		}
		lastOffset = *next
		a["offset"] = lastOffset
	}
}

// nextOf is key's value at the top of an answer, or in the one object at its
// top that carries it.
func nextOf(raw json.RawMessage, key string) (json.RawMessage, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil, false
	}
	if v, ok := top[key]; ok {
		return v, true
	}
	var found json.RawMessage
	n := 0
	for _, v := range top {
		var inner map[string]json.RawMessage
		if len(v) == 0 || v[0] != '{' || json.Unmarshal(v, &inner) != nil {
			continue
		}
		if w, ok := inner[key]; ok {
			found = w
			n++
		}
	}
	if n == 1 {
		return found, true
	}
	return nil, false
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case *int:
		if n != nil {
			return int64(*n), true
		}
	}
	return 0, false
}
