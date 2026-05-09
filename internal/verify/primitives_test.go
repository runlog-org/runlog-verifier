package verify

import (
	"sort"
	"testing"

	runlogschema "github.com/runlog-org/runlog-schema"
	"gopkg.in/yaml.v3"
)

// TestRegisteredPrimitivesMatchesSchema fails CI if registeredPrimitives and
// the schema enum at
// properties.verification.properties.primitives_required.items.enum diverge.
//
// The schema is consumed as a Go module (github.com/runlog-org/runlog-schema)
// — the embedded YAML bytes are always present, so the test no longer
// needs an env-var override or a skip-when-missing branch. See go.mod for
// the replace directive that resolves the module to a sibling clone until
// the schema repo is tagged.
func TestRegisteredPrimitivesMatchesSchema(t *testing.T) {
	data := runlogschema.EntrySchemaYAML()

	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	// Walk: $defs → verification → properties → primitives_required → items → enum
	enumRaw, err := walkSchema(root,
		"$defs", "verification",
		"properties", "primitives_required",
		"items", "enum",
	)
	if err != nil {
		t.Fatalf("schema walk: %v", err)
	}

	rawSlice, ok := enumRaw.([]any)
	if !ok {
		t.Fatalf("expected []any at enum path, got %T", enumRaw)
	}

	schemaSet := make(map[string]struct{}, len(rawSlice))
	for _, v := range rawSlice {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("enum value is not a string: %T %v", v, v)
		}
		schemaSet[s] = struct{}{}
	}

	// Keys in registeredPrimitives but missing from schema.
	var extraInCode []string
	for k := range registeredPrimitives {
		if _, ok := schemaSet[k]; !ok {
			extraInCode = append(extraInCode, k)
		}
	}

	// Keys in schema but missing from registeredPrimitives.
	var missingFromCode []string
	for k := range schemaSet {
		if _, ok := registeredPrimitives[k]; !ok {
			missingFromCode = append(missingFromCode, k)
		}
	}

	sort.Strings(extraInCode)
	sort.Strings(missingFromCode)

	if len(extraInCode) > 0 || len(missingFromCode) > 0 {
		t.Errorf("registeredPrimitives and schema enum have diverged:")
		if len(extraInCode) > 0 {
			t.Errorf("  in code but not in schema: %v", extraInCode)
		}
		if len(missingFromCode) > 0 {
			t.Errorf("  in schema but not in code: %v", missingFromCode)
		}
	}
}

// TestSchemaIsolationsMatchesSchema fails CI if schemaIsolations (unit.go,
// the dispatcher's "is this isolation in the schema enum" view) and the
// schema enum at $defs.verification.properties.isolation.enum diverge.
//
// Drift gap: schemaIsolations is the input to runUnit's
// isolation_unsupported vs isolation_unknown discriminator. When the schema
// adds a new isolation value, a stale schemaIsolations would silently
// classify the new value as `isolation_unknown` (authoring-bug shaped)
// rather than `isolation_unsupported` (pending-driver shaped) — submitters
// would chase a phantom typo report. Mirrors
// TestRegisteredPrimitivesMatchesSchema; same cross-language schema-drift
// pattern as the other parse-YAML-at-test-time assertions in this package.
func TestSchemaIsolationsMatchesSchema(t *testing.T) {
	data := runlogschema.EntrySchemaYAML()

	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	enumRaw, err := walkSchema(root,
		"$defs", "verification",
		"properties", "isolation",
		"enum",
	)
	if err != nil {
		t.Fatalf("schema walk: %v", err)
	}

	rawSlice, ok := enumRaw.([]any)
	if !ok {
		t.Fatalf("expected []any at enum path, got %T", enumRaw)
	}

	schemaSet := make(map[string]struct{}, len(rawSlice))
	for _, v := range rawSlice {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("enum value is not a string: %T %v", v, v)
		}
		schemaSet[s] = struct{}{}
	}

	var extraInCode []string
	for k := range schemaIsolations {
		if _, ok := schemaSet[k]; !ok {
			extraInCode = append(extraInCode, k)
		}
	}
	var missingFromCode []string
	for k := range schemaSet {
		if _, ok := schemaIsolations[k]; !ok {
			missingFromCode = append(missingFromCode, k)
		}
	}
	sort.Strings(extraInCode)
	sort.Strings(missingFromCode)

	if len(extraInCode) > 0 || len(missingFromCode) > 0 {
		t.Errorf("schemaIsolations and schema enum have diverged:")
		if len(extraInCode) > 0 {
			t.Errorf("  in code but not in schema: %v", extraInCode)
		}
		if len(missingFromCode) > 0 {
			t.Errorf("  in schema but not in code: %v", missingFromCode)
		}
	}
}

// walkSchema descends into a nested map[string]any by successive string keys.
func walkSchema(node any, keys ...string) (any, error) {
	cur := node
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, &walkError{key: k, got: cur}
		}
		v, exists := m[k]
		if !exists {
			return nil, &walkError{key: k, missing: true}
		}
		cur = v
	}
	return cur, nil
}

type walkError struct {
	key     string
	got     any
	missing bool
}

func (e *walkError) Error() string {
	if e.missing {
		return "key not found: " + e.key
	}
	return "expected map at key " + e.key
}
