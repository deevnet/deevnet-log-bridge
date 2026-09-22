package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var when = time.Date(2026, 9, 22, 18, 30, 0, 0, time.UTC)

func TestParseTakesTheTenantAndDeviceFromTheTopic(t *testing.T) {
	l, err := Parse("eds/log/lp-stand-01", []byte("lamp on"), when)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if l.Tenant != "eds" {
		t.Errorf("tenant = %q, want eds", l.Tenant)
	}
	if l.Fields[FieldDevice] != "lp-stand-01" {
		t.Errorf("device = %v, want lp-stand-01", l.Fields[FieldDevice])
	}
	if l.Fields[FieldMsg] != "lamp on" {
		t.Errorf("_msg = %v", l.Fields[FieldMsg])
	}
	if l.Fields[FieldTime] != when.Format(time.RFC3339Nano) {
		t.Errorf("_time = %v, want the receive time", l.Fields[FieldTime])
	}
}

// The whole design rests on this. A device may write anything it likes in its
// payload; where the line GOES comes from the topic, which the broker has
// already checked against the publisher's prefix.
func TestAPayloadCannotClaimAnotherTenant(t *testing.T) {
	payload := []byte(`{"tenant":"tdemo","device":"somebody-elses","_msg":"hello"}`)
	l, err := Parse("eds/log/lp-stand-01", payload, when)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if l.Tenant != "eds" {
		t.Fatalf("a payload changed the tenant to %q", l.Tenant)
	}
	if l.Fields[FieldTenant] != "eds" {
		t.Errorf("the stored tenant field is %v, want eds", l.Fields[FieldTenant])
	}
	if l.Fields[FieldDevice] != "lp-stand-01" {
		t.Errorf("the stored device field is %v, want the one from the topic", l.Fields[FieldDevice])
	}
	if l.Fields[FieldMsg] != "hello" {
		t.Errorf("_msg = %v, want the payload's message kept", l.Fields[FieldMsg])
	}
}

func TestParseReadsAJSONPayloadAsFields(t *testing.T) {
	l, err := Parse("eds/log/lp-stand-01", []byte(`{"level":"warn","message":"lamp stuck"}`), when)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if l.Fields["level"] != "warn" {
		t.Errorf("level = %v", l.Fields["level"])
	}
	// A JSON object with no _msg still needs one, or the line reads as empty
	// in every view of the store.
	if l.Fields[FieldMsg] != "lamp stuck" {
		t.Errorf("_msg = %v, want the message field", l.Fields[FieldMsg])
	}
}

func TestParseKeepsAPayloadTimeWhenItHasOne(t *testing.T) {
	l, err := Parse("eds/log/lp-stand-01", []byte(`{"_time":"2026-09-22T00:00:00Z","_msg":"x"}`), when)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if l.Fields[FieldTime] != "2026-09-22T00:00:00Z" {
		t.Errorf("_time = %v, want the payload's", l.Fields[FieldTime])
	}
}

func TestParseRefusesTopicsItCannotAttribute(t *testing.T) {
	cases := map[string]string{
		"no device level":                         "eds/log",
		"not a log topic":                         "eds/lightstand/lp-stand-01/state",
		"a first level that is not a tenant name": "Eds/log/lp-stand-01",
		"a first level too long to be one":        "ninechars/log/lp-stand-01",
		"an empty first level":                    "/log/lp-stand-01",
	}
	for name, topic := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(topic, []byte("x"), when); err == nil {
				t.Fatalf("accepted %q", topic)
			}
		})
	}
}

// One request carries one tenant's lines, because the tenant travels in a
// header. Mixing them would put one tenant's logs in another's partition.
func TestGroupSplitsByTenantAndKeepsOrder(t *testing.T) {
	lines := []Line{
		{Tenant: "eds", Fields: map[string]any{FieldMsg: "1"}},
		{Tenant: "tdemo", Fields: map[string]any{FieldMsg: "2"}},
		{Tenant: "eds", Fields: map[string]any{FieldMsg: "3"}},
	}
	got := Group(lines)
	if len(got) != 2 {
		t.Fatalf("%d batches, want 2", len(got))
	}
	if got[0].Tenant != "eds" || len(got[0].Lines) != 2 {
		t.Errorf("first batch = %+v", got[0])
	}
	if got[0].Lines[0].Fields[FieldMsg] != "1" || got[0].Lines[1].Fields[FieldMsg] != "3" {
		t.Error("order within a tenant was not kept")
	}
	if got[1].Tenant != "tdemo" || len(got[1].Lines) != 1 {
		t.Errorf("second batch = %+v", got[1])
	}
}

func TestBodyIsOneJSONObjectPerLine(t *testing.T) {
	b := Batch{Tenant: "eds", Lines: []Line{
		{Tenant: "eds", Fields: map[string]any{FieldMsg: "one"}},
		{Tenant: "eds", Fields: map[string]any{FieldMsg: "two"}},
	}}
	body, err := b.Body()
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), body)
	}
	for _, l := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(l), &obj); err != nil {
			t.Fatalf("line is not JSON: %q", l)
		}
	}
}

// A line that does not belong to the batch's tenant is a bug, and it must stop
// the request rather than travel under the wrong header.
func TestBodyRefusesAMixedBatch(t *testing.T) {
	b := Batch{Tenant: "eds", Lines: []Line{
		{Tenant: "eds", Fields: map[string]any{FieldMsg: "mine"}},
		{Tenant: "tdemo", Fields: map[string]any{FieldMsg: "not mine"}},
	}}
	if _, err := b.Body(); err == nil {
		t.Fatal("a batch mixing tenants was rendered")
	}
}
