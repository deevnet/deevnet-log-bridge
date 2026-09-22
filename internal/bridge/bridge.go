// Package bridge turns MQTT messages into log lines for the Deevnet log store.
//
// The whole design rests on one fact, and it is worth stating before any code:
// the broker enforces each account's topic prefix (ADR-0012 §10), so a message
// published under "eds/..." was published by an account belonging to eds. The
// first level of the topic is therefore a tenant identity the broker has
// already checked, and it is the only thing this bridge will take a tenant
// from.
//
// Nothing in the payload is identity. A device that sends
// {"tenant":"someone-else"} changes what is stored in the line, not where the
// line goes.
package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TenantRE is the shape of a tenant name: a PVE SDN zone id (ADR-0002), which
// is what a tenant's prefix is. A topic whose first level is not one cannot
// have come from a tenant account, so its message is dropped rather than
// guessed at.
var TenantRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}$`)

// Line is one message, ready to ship.
type Line struct {
	// Tenant decides the partition. It comes from the topic, never the payload.
	Tenant string
	// Fields is what the store receives, one JSON object per line.
	Fields map[string]any
}

// Reserved names this bridge sets itself. A payload carrying any of them is
// not trusted for them: the values here are what the broker and this process
// observed, and a device cannot overwrite them by choosing a field name.
const (
	FieldMsg    = "_msg"
	FieldTime   = "_time"
	FieldTenant = "tenant"
	FieldDevice = "device"
	FieldTopic  = "topic"
)

// Parse turns one message into a Line.
//
// The topic is <tenant>/log/<device...>. The tenant is the first level and the
// device is the last; anything between is kept in the topic field rather than
// interpreted, because this bridge does not own a tenant's topic tree.
func Parse(topic string, payload []byte, received time.Time) (Line, error) {
	levels := strings.Split(topic, "/")
	if len(levels) < 3 {
		return Line{}, fmt.Errorf("topic %q has too few levels to be <tenant>/log/<device>", topic)
	}
	tenant, kind, device := levels[0], levels[1], levels[len(levels)-1]
	if !TenantRE.MatchString(tenant) {
		return Line{}, fmt.Errorf("topic %q does not start with a tenant name", topic)
	}
	if kind != "log" {
		return Line{}, fmt.Errorf("topic %q is not a log topic", topic)
	}
	if device == "" {
		return Line{}, fmt.Errorf("topic %q names no device", topic)
	}

	fields := fieldsFrom(payload)

	// Set last, and unconditionally: these are observed, not claimed.
	fields[FieldTenant] = tenant
	fields[FieldDevice] = device
	fields[FieldTopic] = topic
	if _, ok := fields[FieldTime]; !ok {
		// The broker's receive time, because a device's clock is not to be
		// relied on and a line with no time at all is stored at ingest time
		// anyway - this at least records when it arrived.
		fields[FieldTime] = received.UTC().Format(time.RFC3339Nano)
	}
	return Line{Tenant: tenant, Fields: fields}, nil
}

// fieldsFrom reads the payload as a JSON object when it is one, and as text
// otherwise. A device that sends plain lines is the common case, and it should
// not have to learn JSON to be logged.
func fieldsFrom(payload []byte) map[string]any {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var obj map[string]any
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			if _, ok := obj[FieldMsg]; !ok {
				// A JSON object with no _msg still needs one: VictoriaLogs
				// stores the line's message from that field, and a line with
				// none reads as empty in every view.
				obj[FieldMsg] = firstString(obj, "message", "msg", "text", "log")
			}
			return obj
		}
	}
	return map[string]any{FieldMsg: string(trimmed)}
}

func firstString(obj map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := obj[n].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// Batch is a set of lines for ONE tenant. Lines for different tenants are
// never mixed, because the tenant travels in a header: one request carries one
// tenant's lines or it carries the wrong ones.
type Batch struct {
	Tenant string
	Lines  []Line
}

// Body renders the batch as the store's JSON stream format: one object per
// line.
func (b Batch) Body() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range b.Lines {
		if l.Tenant != b.Tenant {
			return nil, fmt.Errorf("a line for %q is in a batch for %q", l.Tenant, b.Tenant)
		}
		if err := enc.Encode(l.Fields); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// Group splits lines by tenant, preserving order within each tenant.
func Group(lines []Line) []Batch {
	var order []string
	byTenant := map[string][]Line{}
	for _, l := range lines {
		if _, seen := byTenant[l.Tenant]; !seen {
			order = append(order, l.Tenant)
		}
		byTenant[l.Tenant] = append(byTenant[l.Tenant], l)
	}
	out := make([]Batch, 0, len(order))
	for _, t := range order {
		out = append(out, Batch{Tenant: t, Lines: byTenant[t]})
	}
	return out
}
