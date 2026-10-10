package core

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

type verbatimJSON string

func (v *verbatimJSON) UnmarshalJSON(raw []byte) error {
	*v = verbatimJSON(raw)
	return nil
}

func TestIntegerDecodingPreservesOtherValues(t *testing.T) {
	type input struct {
		N      int             `json:"n"`
		X      float64         `json:"x"`
		Raw    json.RawMessage `json:"raw"`
		Custom verbatimJSON    `json:"custom"`
		Number json.Number     `json:"number"`
		Any    any             `json:"any"`
	}
	raw := json.RawMessage(`{ "n": 2.0, "x": -0.0, "raw": { "z": 1, "a": 2.0 }, "custom": { "value": 3e0 }, "number": 4.0, "any": -0.0 }`)
	check := func(t *testing.T, got input) {
		t.Helper()
		if got.N != 2 || !math.Signbit(got.X) || !math.Signbit(got.Any.(float64)) {
			t.Fatalf("decoded values = %+v; want integer 2 and negative zeros", got)
		}
		if string(got.Raw) != `{ "z": 1, "a": 2.0 }` || string(got.Custom) != `{ "value": 3e0 }` || got.Number.String() != "4.0" {
			t.Fatalf("opaque values changed: raw=%s custom=%s number=%s", got.Raw, got.Custom, got.Number)
		}
	}
	tool := Func("decode", "decode", func(_ context.Context, got input) (string, error) {
		check(t, got)
		return "ok", nil
	})
	result, err := tool.Execute(context.Background(), raw)
	if err != nil || result.IsError {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	got, err := decodeTyped[input](raw)
	if err != nil {
		t.Fatal(err)
	}
	check(t, got)

	// The public Decode path also preserves float signs and RawMessage bytes.
	type output struct {
		N   int             `json:"n"`
		X   float64         `json:"x"`
		Raw json.RawMessage `json:"raw"`
	}
	decoded, err := Decode[output](RunResult{StructuredOutput: json.RawMessage(`{"n":2.0,"x":-0.0,"raw":{ "a": 2.0 }}`)})
	if err != nil || decoded.N != 2 || !math.Signbit(decoded.X) || string(decoded.Raw) != `{ "a": 2.0 }` {
		t.Fatalf("Decode = %+v, %v", decoded, err)
	}
}

func TestIntegerDecodingRejectsMalformedJSON(t *testing.T) {
	type input struct {
		N int `json:"n"`
	}
	called := false
	tool := Func("decode", "decode", func(context.Context, input) (string, error) {
		called = true
		return "should not execute", nil
	})
	for _, raw := range []string{`{"n":2.0} garbage`, `{"n":2.0} {}`, `{"n":2.0} null`, `{"n":2.0,}`, `{"n":2.5}`, `{"n":1e30}`} {
		t.Run(raw, func(t *testing.T) {
			called = false
			result, err := tool.Execute(context.Background(), json.RawMessage(raw))
			if err != nil || !result.IsError || result.Effect.Status != EffectNotApplied || called {
				t.Fatalf("Execute = %+v, %v, handler called=%t", result, err, called)
			}
			if _, err := decodeTyped[input](json.RawMessage(raw)); err == nil {
				t.Fatal("typed decoder accepted invalid input")
			}
		})
	}
}

func TestIntegerNormalizationLocations(t *testing.T) {
	type embedded struct {
		Count int `json:"count"`
	}
	type input struct {
		embedded
		Pointer *int           `json:"pointer"`
		Array   [2]uint8       `json:"array"`
		Slice   []*int         `json:"slice"`
		Map     map[string]int `json:"map"`
		Ignored int            `json:"-"`
	}
	raw := json.RawMessage(`{ "COUNT": 2.0, "pointer": 3e0, "array": [4.0,5e0], "slice": [null,6.0], "map": {"k":7e0}, "Ignored": 8.0, "unknown": {"n":9.0} }`)
	want := `{ "COUNT": 2, "pointer": 3, "array": [4,5], "slice": [null,6], "map": {"k":7}, "Ignored": 8.0, "unknown": {"n":9.0} }`
	got := canonicalIntegralNumbers(raw, reflect.TypeFor[input]())
	if string(got) != want {
		t.Fatalf("normalized = %s; want %s", got, want)
	}
	var decoded input
	if err := json.Unmarshal(got, &decoded); err != nil || decoded.Count != 2 || *decoded.Pointer != 3 || decoded.Array != [2]uint8{4, 5} || *decoded.Slice[1] != 6 || decoded.Map["k"] != 7 {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}
}

func TestIntegerNormalizationFieldDominance(t *testing.T) {
	type first struct{ N int }
	type second struct{ N int }
	type ambiguous struct {
		first
		second
	}
	type shallow struct {
		first
		N float64
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[ambiguous](), reflect.TypeFor[shallow]()} {
		raw := json.RawMessage(`{"N":-0.0}`)
		if got := canonicalIntegralNumbers(raw, typ); string(got) != string(raw) {
			t.Fatalf("%v normalized ignored/non-integer field: %s", typ, got)
		}
	}
}

func TestIntegerNormalizationMatchesJSONFieldFolding(t *testing.T) {
	// encoding/json chooses the first field in declaration/index order for
	// a folded match, but exact spelling always wins (including Unicode).
	type floatFirst struct {
		F float64 `json:"name"`
		N int     `json:"NAME"`
	}
	raw := json.RawMessage(`{"NaMe":-0.0}`)
	var baseline, got floatFirst
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(canonicalIntegralNumbers(raw, reflect.TypeFor[floatFirst]()), &got); err != nil || !math.Signbit(got.F) || !math.Signbit(baseline.F) {
		t.Fatalf("folded float match = %+v, baseline=%+v, %v", got, baseline, err)
	}
	type integerFirst struct {
		N int     `json:"NAME"`
		F float64 `json:"name"`
	}
	var integer integerFirst
	if err := json.Unmarshal(canonicalIntegralNumbers([]byte(`{"NaMe":2.0}`), reflect.TypeFor[integerFirst]()), &integer); err != nil || integer.N != 2 {
		t.Fatalf("folded integer match = %+v, %v", integer, err)
	}
	if err := json.Unmarshal(canonicalIntegralNumbers([]byte(`{"name":-0.0}`), reflect.TypeFor[integerFirst]()), &integer); err != nil || !math.Signbit(integer.F) {
		t.Fatalf("exact float match = %+v, %v", integer, err)
	}
	type unicodeInput struct {
		N int `json:"K"`
	}
	var unicodeValue unicodeInput
	if err := json.Unmarshal(canonicalIntegralNumbers([]byte(`{"K":2.0}`), reflect.TypeFor[unicodeInput]()), &unicodeValue); err != nil || unicodeValue.N != 2 {
		t.Fatalf("Unicode fold match = %+v, %v", unicodeValue, err)
	}
}

func TestCanonicalIntegerLiteralIsExactAndBounded(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		ok          bool
	}{
		{"2.0", "2", true},
		{"1000e-3", "1", true},
		{"0.01e21", "10000000000000000000", true},
		{"18446744073709551615.0", "18446744073709551615", true},
		{"-9223372036854775808.0", "-9223372036854775808", true},
		{"-0.0", "0", true},
		{"0e9999999999999999999999", "0", true},
		{"1e9999999999999999999999", "", false},
		{"1e-9999999999999999999999", "", false},
		{"1e21", "", false},
		{"2.5", "", false},
		{"1e-3", "", false},
	} {
		got, ok := canonicalIntegerLiteral(tc.input)
		if got != tc.want || ok != tc.ok {
			t.Errorf("canonicalIntegerLiteral(%s) = %q, %t; want %q, %t", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}
