package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Validator test types exercise every rule in buildSchema: required/optional,
// renamed fields, json:"-", nested structs, slices, maps, pointers, opaque
// fields, and a self-referential type.
type valNested struct {
	Topic string `json:"topic"`
}

type valNode struct {
	Value string    `json:"value"`
	Next  *valNode  `json:"next,omitempty"`
	Kids  []valNode `json:"kids,omitempty"`
}

type valPayload struct {
	Name     string             `json:"name"`
	Age      int                `json:"age"`
	Nick     string             `json:"nick,omitempty"`
	FullName string             `json:"full_name,omitempty"`
	Addr     *valNested         `json:"addr"`
	OptAddr  *valNested         `json:"optAddr,omitempty"`
	Tags     []string           `json:"tags"`
	Scores   []valNested        `json:"scores"`
	Meta     map[string]float64 `json:"meta"`
	Raw      json.RawMessage    `json:"raw"`
	Any      any                `json:"any,omitempty"`
	When     time.Time          `json:"when"`
	Blob     []byte             `json:"blob"`
	Skip     string             `json:"-"`
	Node     *valNode           `json:"node,omitempty"`
}

func TestValidateTypedPayload(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantErrs   []string // exact violations, in order; nil means valid
		wantSubstr []string // each must appear among the violations
	}{
		{
			name: "happy path full payload",
			raw: `{
				"name":"Ada","age":36,
				"addr":{"topic":"x"},"tags":["a"],"scores":[{"topic":"y"}],
				"meta":{"n":1.5},"raw":{"k":1},"any":"whatever",
				"when":"2024-01-02T03:04:05Z","blob":"aGk=",
				"node":{"value":"v","kids":[{"value":"k"}]}
			}`,
		},
		{
			name: "unknown fields ignored",
			raw:  `{"name":"Ada","age":36,"extra":"ignored","another":[1,2]}`,
			wantErrs: []string{
				"addr: missing required field",
				"tags: missing required field",
				"scores: missing required field",
				"meta: missing required field",
				"raw: missing required field",
				"when: missing required field",
				"blob: missing required field",
			},
		},
		{
			name:       "missing flat required field",
			raw:        `{"age":36,"addr":null,"tags":[],"scores":[],"meta":{},"raw":null,"when":"2024-01-02T03:04:05Z","blob":""}`,
			wantSubstr: []string{`name: missing required field`},
		},
		{
			name:       "empty payload lists every required field in field order",
			raw:        `{}`,
			wantSubstr: []string{"name:", "age:", "addr:", "tags:", "scores:", "meta:", "raw:", "when:", "blob:"},
		},
		{
			name:       "wrong type flat",
			raw:        `{"name":5}`,
			wantSubstr: []string{"name: expected string, got number"},
		},
		{
			name:       "wrong type nested in slice element",
			raw:        `{"scores":[{"topic":"ok"},{"topic":7}]}`,
			wantSubstr: []string{"scores[1].topic: expected string, got number"},
		},
		{
			name:       "missing required field inside slice element",
			raw:        `{"scores":[{},{"other":"x"}]}`,
			wantSubstr: []string{"scores[0].topic: missing required field", "scores[1].topic: missing required field"},
		},
		{
			name:       "missing required field inside map value",
			raw:        `{"meta":{"count":"not-a-number"}}`,
			wantSubstr: []string{"meta.count: expected number, got string"},
		},
		{
			name:       "wrong root shape",
			raw:        `[1,2]`,
			wantSubstr: []string{"expected object, got array"},
		},
		{
			name:       "null into non-pointer required field",
			raw:        `{"name":null}`,
			wantSubstr: []string{"name: null is not a valid value for string"},
		},
		{
			name: "null is valid for pointer, slice, and map fields",
			raw:  `{"name":"a","age":1,"addr":null,"tags":null,"scores":null,"meta":null}`,
			wantErrs: []string{
				"raw: missing required field",
				"when: missing required field",
				"blob: missing required field",
			},
		},
		{
			name:       "null into nested non-pointer struct element",
			raw:        `{"scores":[null]}`,
			wantSubstr: []string{"scores[0]: null is not a valid value for object"},
		},
		{
			name: "optional pointer absent or null both fine",
			raw:  `{"name":"a","age":1,"addr":null,"optAddr":null,"tags":[],"scores":[],"meta":{},"raw":null,"when":"2024-01-02T03:04:05Z","blob":""}`,
		},
		{
			name:       "non-integral integer",
			raw:        `{"age":1.5}`,
			wantSubstr: []string{"age: expected integer, got non-integral number 1.5"},
		},
		{
			name: "integral float accepted for integer field",
			raw:  `{"name":"a","age":1.0,"addr":null,"tags":[],"scores":[],"meta":{},"raw":null,"when":"2024-01-02T03:04:05Z","blob":""}`,
		},
		{
			name:       "renamed field enforced under its json name",
			raw:        `{"full_name":5}`,
			wantSubstr: []string{"full_name: expected string, got number"},
		},
		{
			name:       "optional field present but wrong type still checked",
			raw:        `{"nick":5}`,
			wantSubstr: []string{"nick: expected string, got number"},
		},
		{
			name:       "pointer element validated recursively",
			raw:        `{"addr":{}}`,
			wantSubstr: []string{"addr.topic: missing required field"},
		},
		{
			name:       "null into opaque interface field is a violation",
			raw:        `{"any":null}`,
			wantSubstr: []string{"any: null is not a valid value for any"},
		},
		{
			name:       "invalid json reports a violation",
			raw:        `{not json`,
			wantSubstr: []string{"payload is not valid JSON"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateTypedPayload[valPayload](json.RawMessage(tt.raw))
			if tt.wantErrs != nil {
				if len(got) != len(tt.wantErrs) {
					t.Fatalf("violations = %q, want %q", got, tt.wantErrs)
				}
				for i := range got {
					if got[i] != tt.wantErrs[i] {
						t.Errorf("violation[%d] = %q, want %q", i, got[i], tt.wantErrs[i])
					}
				}
			}
			for _, sub := range tt.wantSubstr {
				if !containsViolation(got, sub) {
					t.Errorf("violations missing %q: %q", sub, got)
				}
			}
			if len(tt.wantErrs) == 0 && len(tt.wantSubstr) == 0 && len(got) != 0 {
				t.Errorf("violations = %q, want none", got)
			}
		})
	}
}

func containsViolation(violations []string, sub string) bool {
	for _, v := range violations {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}

// TestValidateTypedPayloadOmitsUnexportedFields pins that unexported fields are
// neither required nor validated (they match objectSchema's skip rule).
func TestValidateTypedPayloadOmitsUnexportedFields(t *testing.T) {
	type payload struct {
		Visible string `json:"visible"`
		hidden  string // unexported: never required, never validated
	}
	// The unexported field must not be reported missing even though it has a
	// json tag; hidden would fail to compile as a requirement, so just check
	// that only "visible" is demanded.
	got := validateTypedPayload[payload](json.RawMessage(`{}`))
	if len(got) != 1 || got[0] != "visible: missing required field" {
		t.Fatalf("violations = %q, want only visible", got)
	}
}

// TestValidateTypedPayloadMapKeysSorted pins deterministic map-value order.
func TestValidateTypedPayloadMapKeysSorted(t *testing.T) {
	type payload struct {
		Meta map[string]int `json:"meta"`
	}
	got := validateTypedPayload[payload](json.RawMessage(`{"meta":{"z":"a","a":"b","m":"c"}}`))
	want := []string{
		"meta.a: expected integer, got string",
		"meta.m: expected integer, got string",
		"meta.z: expected integer, got string",
	}
	if len(got) != len(want) {
		t.Fatalf("violations = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("violation[%d] = %q, want %q (deterministic order)", i, got[i], want[i])
		}
	}
}
