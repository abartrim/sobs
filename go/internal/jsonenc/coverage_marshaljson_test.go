package jsonenc

// coverage_marshaljson_test.go — oracle-anchored unit test for the order-preserving JSON encoder's
// stdlib entrypoint.
//
// Target:
//   TESTED:
//     (*Object).MarshalJSON  (jsonenc.go:89)
//
// MarshalJSON implements encoding/json.Marshaler so that stdlib json.Marshal / json.MarshalIndent
// serialize an *Object in INSERTION order (a plain Go map would sort keys). It delegates to the
// package Encode with {SortKeys:false, EnsureASCII:true, ItemSep:",", KeySep:":"} — i.e. compact
// separators, non-ASCII escaped to \uXXXX, NO trailing newline.
//
// Oracle anchor: app.py's order-preserving indent-2 export payloads, e.g.
//   json.dumps(payload, ensure_ascii=False, indent=2)         (app.py:10565, 15747, 19036, 19213…)
//   json.dumps(payload, indent=2)                             (app.py:22843, 24875)
// where the Python dict's natural insertion order is preserved by json.dumps. The Go side reaches
// this order-preserving behavior by feeding *Object through json.MarshalIndent, which calls
// MarshalJSON to get the compact ordered bytes and then re-indents them. This test pins the
// compact, ordered, ASCII-safe bytes MarshalJSON emits, and that json.Marshal/MarshalIndent route
// through it (preserving order where a map would have sorted).

import (
	"encoding/json"
	"testing"
)

func TestMarshalJSON_PreservesInsertionOrder(t *testing.T) {
	// Keys deliberately NOT in sorted order, so a sorting encoder would reorder them.
	o := NewObject().
		Set("zeta", 1).
		Set("alpha", 2).
		Set("middle", 3)

	// 1) Direct method call.
	got, err := o.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON returned error: %v", err)
	}
	want := `{"zeta":1,"alpha":2,"middle":3}`
	if string(got) != want {
		t.Fatalf("MarshalJSON = %q; want %q (insertion order, compact, no trailing newline)", got, want)
	}

	// 2) Via stdlib json.Marshal — must route through MarshalJSON and produce identical bytes
	//    (insertion order preserved; a map[string]any here would have sorted alpha<middle<zeta).
	viaStd, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("json.Marshal(*Object) error: %v", err)
	}
	if string(viaStd) != want {
		t.Fatalf("json.Marshal(*Object) = %q; want %q", viaStd, want)
	}

	// 3) Consistency with the package Encode using the same options.
	viaEncode := Encode(o, Options{SortKeys: false, EnsureASCII: true, ItemSep: ",", KeySep: ":"})
	if string(viaEncode) != string(got) {
		t.Fatalf("MarshalJSON (%q) disagrees with Encode same-opts (%q)", got, viaEncode)
	}
}

func TestMarshalJSON_MarshalIndentReindentsInOrder(t *testing.T) {
	// json.MarshalIndent calls MarshalJSON for the compact bytes, then re-indents. The result must
	// keep insertion order — this is the order-preserving indent-2 export payload behavior that
	// mirrors app.py's json.dumps(payload, indent=2) (e.g. app.py:24875).
	o := NewObject().Set("b", 1).Set("a", 2)
	got, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent error: %v", err)
	}
	want := "{\n  \"b\": 1,\n  \"a\": 2\n}"
	if string(got) != want {
		t.Fatalf("MarshalIndent(*Object) = %q; want %q (order preserved, not sorted)", got, want)
	}
}

func TestMarshalJSON_EnsureASCIIAndNesting(t *testing.T) {
	cases := []struct {
		name  string
		build func() *Object
		want  string
	}{
		{
			name:  "empty object",
			build: func() *Object { return NewObject() },
			want:  `{}`,
		},
		{
			// EnsureASCII:true (MarshalJSON's pinned option) escapes non-ASCII to \uXXXX,
			// exactly like CPython json.dumps(ensure_ascii=True). U+00E9 -> the 6 ASCII bytes
			// é. want is built with an interpreted-string escape so the literal contains
			// a real backslash-u sequence, not the é rune.
			name:  "non-ASCII escaped to \\uXXXX (EnsureASCII)",
			build: func() *Object { return NewObject().Set("name", "café") },
			want:  "{\"name\":\"caf\\u00e9\"}",
		},
		{
			name: "nested *Object preserves its own insertion order",
			build: func() *Object {
				return NewObject().
					Set("outer", "x").
					Set("inner", NewObject().Set("z", 1).Set("y", 2))
			},
			want: `{"outer":"x","inner":{"z":1,"y":2}}`,
		},
		{
			name: "mixed value types",
			build: func() *Object {
				return NewObject().
					Set("s", "v").
					Set("n", 42).
					Set("b", true).
					Set("arr", []any{1, 2, 3})
			},
			want: `{"s":"v","n":42,"b":true,"arr":[1,2,3]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := c.build()
			got, err := o.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON error: %v", err)
			}
			if string(got) != c.want {
				t.Fatalf("MarshalJSON = %q; want %q", got, c.want)
			}
			// json.Marshal must agree (it dispatches to MarshalJSON).
			viaStd, err := json.Marshal(o)
			if err != nil {
				t.Fatalf("json.Marshal error: %v", err)
			}
			if string(viaStd) != c.want {
				t.Fatalf("json.Marshal = %q; want %q", viaStd, c.want)
			}
		})
	}
}
