// Package contract holds Interval's JSON Schemas for what Velox speaks (Interval's
// ADR-0040, ADR-0041): the LLD scenario and result lines (lld) and judge jobs
// (judgejob). The Go types beside those packages are generated from the same zod
// definitions by Interval's `pnpm contracts:velox`; this test fails when either copy
// changes without the other.
package contract

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	judge "github.com/rishik92/velox/judgejob"
	"github.com/rishik92/velox/lld"
)

type schema struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
	AnyOf      []schema                   `json:"anyOf"`
}

// fields gives a struct's JSON names, and those it may omit.
func fields(t reflect.Type) (names, optional []string) {
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		names = append(names, name)
		if strings.Contains(opts, "omitempty") {
			optional = append(optional, name)
		}
	}
	sort.Strings(names)
	sort.Strings(optional)
	return names, optional
}

func TestGeneratedTypesMatchTheSchemas(t *testing.T) {
	for file, typ := range map[string]reflect.Type{
		"lld.scenario": reflect.TypeOf(lld.Scenario{}),
		"lld.result":   reflect.TypeOf(lld.ResultLine{}),
		"lld.check":    reflect.TypeOf(lld.Check{}),
		"judge.job":    reflect.TypeOf(judge.JudgeJob{}),
		"judge.result": reflect.TypeOf(judge.JudgeResult{}),
	} {
		raw, err := os.ReadFile("schema/" + file + ".schema.json")
		if err != nil {
			t.Fatal(err)
		}
		var s schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		// A union (a result line) is one struct holding every variant's fields.
		props, required := map[string]bool{}, map[string]int{}
		variants := s.AnyOf
		if len(variants) == 0 {
			variants = []schema{s}
		}
		for _, v := range variants {
			for k := range v.Properties {
				props[k] = true
			}
			for _, k := range v.Required {
				required[k]++
			}
		}
		var want, wantOptional []string
		for k := range props {
			want = append(want, k)
			if required[k] != len(variants) {
				wantOptional = append(wantOptional, k)
			}
		}
		sort.Strings(want)
		sort.Strings(wantOptional)
		got, gotOptional := fields(typ)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: fields %v, the schema has %v: run `pnpm contracts:velox` in Interval", file, got, want)
		}
		if !reflect.DeepEqual(gotOptional, wantOptional) {
			t.Errorf("%s: optional %v, the schema has %v", file, gotOptional, wantOptional)
		}
	}
}
