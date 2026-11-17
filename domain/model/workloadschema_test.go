package model

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

var schemaFingerprint = strings.Repeat("ab", 32)

func TestNewWorkloadSchemaKeepsItsFields(t *testing.T) {
	raw := []byte(`{"type":"object"}`)
	at := time.Date(2026, 11, 17, 10, 0, 0, 5, time.FixedZone("x", 3600))
	w, err := NewWorkloadSchema(schemaFingerprint, "solti.io/v1", "Subprocess", raw, "agent-1", at)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID() != schemaFingerprint || w.APIVersion() != "solti.io/v1" || w.Kind() != "Subprocess" || w.FirstAgentID() != "agent-1" || !w.FirstSeenAt().Equal(at) {
		t.Fatalf("fields: %+v", w)
	}
	if !w.CreatedAt().Equal(at) || !w.UpdatedAt().Equal(at) {
		t.Fatal("created and updated times are not the first-seen time")
	}
	raw[0] = 'X'
	if !bytes.Equal(w.Schema(), []byte(`{"type":"object"}`)) {
		t.Fatal("the constructor keeps the caller's slice")
	}
}

func TestWorkloadSchemaSchemaReturnsACopy(t *testing.T) {
	w, err := NewWorkloadSchema(schemaFingerprint, "v", "k", []byte("abc"), "a", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	w.Schema()[0] = 'X'
	if string(w.Schema()) != "abc" {
		t.Fatal("Schema() aliases the entry")
	}
	c := w.Clone()
	c.schema[0] = 'Y'
	if string(w.Schema()) != "abc" {
		t.Fatal("Clone aliases the entry")
	}
	if (*WorkloadSchema)(nil).Clone() != nil {
		t.Fatal("Clone of nil")
	}
}

func TestNewWorkloadSchemaRefusals(t *testing.T) {
	at := time.Unix(1, 0)
	for name, tc := range map[string]struct {
		fingerprint, apiVersion, kind, agent string
		schema                               []byte
		at                                   time.Time
	}{
		"short fingerprint":     {strings.Repeat("a", 63), "v", "k", "a", []byte("s"), at},
		"long fingerprint":      {strings.Repeat("a", 65), "v", "k", "a", []byte("s"), at},
		"uppercase fingerprint": {strings.Repeat("A", 64), "v", "k", "a", []byte("s"), at},
		"non-hex fingerprint":   {strings.Repeat("g", 64), "v", "k", "a", []byte("s"), at},
		"empty fingerprint":     {"", "v", "k", "a", []byte("s"), at},
		"empty API version":     {schemaFingerprint, "", "k", "a", []byte("s"), at},
		"empty kind":            {schemaFingerprint, "v", "", "a", []byte("s"), at},
		"empty agent":           {schemaFingerprint, "v", "k", "", []byte("s"), at},
		"empty schema":          {schemaFingerprint, "v", "k", "a", nil, at},
		"invalid UTF-8 kind":    {schemaFingerprint, "v", "\xff", "a", []byte("s"), at},
		"Unix epoch":            {schemaFingerprint, "v", "k", "a", []byte("s"), time.Unix(0, 0)},
		"zero time":             {schemaFingerprint, "v", "k", "a", []byte("s"), time.Time{}},
		"time beyond nanos":     {schemaFingerprint, "v", "k", "a", []byte("s"), time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(name, func(t *testing.T) {
			if w, err := NewWorkloadSchema(tc.fingerprint, tc.apiVersion, tc.kind, tc.schema, tc.agent, tc.at); err == nil {
				t.Fatalf("accepted: %+v", w)
			}
		})
	}
}
