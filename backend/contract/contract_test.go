// Package contract holds Interval's JSON Schemas for what Velox speaks (Interval's
// ADR-0040, ADR-0041): the LLD scenario and result lines (lld) and judge jobs
// (judgejob). The Go types beside those packages are generated from the same zod
// definitions by Interval's `pnpm contracts:velox`. This test holds their shape to the
// schemas (fields, optional fields, nested objects and lists); the rules themselves
// (bounds, patterns) are generated into Validate from the same source.
package contract

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	judge "github.com/rishik92/velox/judgejob"
	"github.com/rishik92/velox/lld"
)

type schema struct {
	Properties map[string]schema `json:"properties"`
	Required   []string          `json:"required"`
	AnyOf      []schema          `json:"anyOf"`
	Items      *schema           `json:"items"`
}

// object gives a schema node's fields and which may be absent; a union (a result line)
// is one Go struct holding every variant's fields, each optional unless all require it.
func (s schema) object() (props map[string]schema, optional map[string]bool, ok bool) {
	variants := s.AnyOf
	if len(variants) == 0 {
		if s.Properties == nil {
			return nil, nil, false
		}
		variants = []schema{s}
	}
	props, required := map[string]schema{}, map[string]int{}
	for _, v := range variants {
		for k, p := range v.Properties {
			props[k] = p
		}
		for _, k := range v.Required {
			required[k]++
		}
	}
	optional = map[string]bool{}
	for k := range props {
		optional[k] = required[k] != len(variants)
	}
	return props, optional, true
}

// match holds a Go struct to a schema object: the same fields, the same optional ones,
// and every nested object or list of objects the same way, all the way down.
func match(t *testing.T, at string, s schema, typ reflect.Type) {
	t.Helper()
	props, optional, _ := s.object()
	fields := map[string]reflect.StructField{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		fields[name] = f
		if _, inSchema := props[name]; !inSchema {
			t.Errorf("%s.%s: in Go, not in the schema: run `pnpm contracts:velox` in Interval", at, name)
			continue
		}
		if strings.Contains(opts, "omitempty") != optional[name] {
			t.Errorf("%s.%s: optional in Go %v, in the schema %v", at, name, !optional[name], optional[name])
		}
	}
	for name, p := range props {
		f, ok := fields[name]
		if !ok {
			t.Errorf("%s.%s: in the schema, not in Go", at, name)
			continue
		}
		// Through pointers and lists to the struct a nested object becomes.
		node, ft := p, f.Type
		for {
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Slice && node.Items != nil {
				node, ft = *node.Items, ft.Elem()
				continue
			}
			break
		}
		if _, _, isObject := node.object(); isObject {
			if ft.Kind() != reflect.Struct {
				t.Errorf("%s.%s: an object in the schema, %s in Go", at, name, ft)
				continue
			}
			match(t, at+"."+name, node, ft)
		}
	}
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
		match(t, file, s, typ)
	}
}
