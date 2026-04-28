package verify

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

// TestSchemaOracle exercises the schema-as-test-oracle slice of F31:
// for each canonical sample entry, both
//   (a) the entry parses cleanly through verify.Run, AND
//   (b) the entry validates against runlog-schema/entry.schema.yaml.
// Either side breaking — verifier accepting a non-schema-valid entry, or a
// schema-valid entry crashing the loader — is a test failure.
//
// The schema lives in a sibling repo (runlog-org/runlog-schema). Set
// RUNLOG_SCHEMA_PATH to point at entry.schema.yaml; default assumes a
// sibling clone at ../../../runlog-schema/. When the schema is missing AND
// the env var is unset, the entire test skips so a standalone verifier
// checkout still runs the rest of the suite. Mirrors the existing
// RUNLOG_SCHEMA_PATH pattern in primitives_test.go.
//
// The compiler is fed the schema as an in-memory JSON resource — no network
// calls happen during this test.
func TestSchemaOracle(t *testing.T) {
	schemaPath := os.Getenv("RUNLOG_SCHEMA_PATH")
	if schemaPath == "" {
		schemaPath = filepath.Join("..", "..", "..", "runlog-schema", "entry.schema.yaml")
	}
	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("schema not found at %s; clone runlog-org/runlog-schema as a sibling or set RUNLOG_SCHEMA_PATH", schemaPath)
		}
		t.Fatalf("read schema: %v", err)
	}

	// Convert YAML schema → JSON in-memory; the jsonschema/v5 compiler
	// expects a JSON document on AddResource. The schema's $id is
	// https://runlog.org/schemas/entry/v1.json — register it under that
	// URL so the compiler resolves the document locally and never reaches
	// for the network.
	schemaJSON, err := yamlBytesToJSONBytes(schemaBytes)
	if err != nil {
		t.Fatalf("convert schema YAML to JSON: %v", err)
	}

	const schemaURL = "https://runlog.org/schemas/entry/v1.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaURL, strings.NewReader(string(schemaJSON))); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	// Sample-entry table.
	//
	// Local testdata/ fixtures are always present (committed alongside
	// this test); seeds under runlog/server/seeds/ live in a sibling
	// repo and are skipped per-row when the sibling clone is missing.
	seedsDir := filepath.Join("..", "..", "..", "runlog", "server", "seeds")
	cases := []struct {
		name string
		path string
		// shape names the verification.type we expect — guards against
		// silent file renames pulling in a different shape.
		shape string
	}{
		{
			name:  "assertion_only/oauth2-pkce-plain",
			path:  filepath.Join(seedsDir, "oauth2-pkce-code-challenge-method-plain-defeats-protection.yaml"),
			shape: "assertion_only",
		},
		{
			name:  "unit/pyyaml-shape",
			path:  filepath.Join("testdata", "pyyaml-shape.yaml"),
			shape: "unit",
		},
		{
			name:  "integration-replay/lets-encrypt-duplicate-cert",
			path:  filepath.Join(seedsDir, "lets-encrypt-duplicate-certificate-limit-five-per-week.yaml"),
			shape: "integration",
		},
		{
			name:  "integration-reexecute/shell-uppercase",
			path:  filepath.Join("testdata", "reexecute-shape-poc.yaml"),
			shape: "integration",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
			if err != nil {
				if os.IsNotExist(err) {
					t.Skipf("sample entry not found at %s (sibling clone missing?)", tc.path)
				}
				t.Fatalf("read entry: %v", err)
			}

			// (a) Validate the raw entry against the schema.
			//
			// The compiler.Validate path expects raw JSON values
			// (map[string]any with string keys, []any, json.Number,
			// string, bool, nil). YAML→JSON round-trip guarantees
			// that shape regardless of yaml.v3 quirks.
			entryJSON, err := yamlBytesToJSONBytes(data)
			if err != nil {
				t.Fatalf("convert entry YAML to JSON: %v", err)
			}
			var entryDoc any
			dec := json.NewDecoder(strings.NewReader(string(entryJSON)))
			dec.UseNumber()
			if err := dec.Decode(&entryDoc); err != nil {
				t.Fatalf("decode entry JSON: %v", err)
			}

			if err := schema.Validate(entryDoc); err != nil {
				t.Fatalf("schema rejected %s:\n%v", tc.path, err)
			}

			// (b) Run the entry through the verifier loader. We invoke
			// verify.Run — the entry-point all CLI / library callers
			// use — so any future loader change is exercised here.
			res, runErr := Run(data)
			if runErr != nil {
				// Run returns errors only for *programming* failures
				// (YAML parse, empty entry). A schema-valid entry
				// must not trigger one.
				t.Fatalf("verify.Run error on schema-valid entry %s: %v", tc.path, runErr)
			}

			// Cross-check: the loader's view of the entry agrees with
			// the schema-validated raw map on the load-bearing fields.
			rawMap, ok := entryDoc.(map[string]any)
			if !ok {
				t.Fatalf("expected top-level map, got %T", entryDoc)
			}

			if got, want := res.UnitID, asString(rawMap["unit_id"]); got != want {
				t.Errorf("unit_id: loader=%q schema-raw=%q", got, want)
			}
			if got, want := res.Tier, tc.shape; got != want {
				t.Errorf("verification.type: loader=%q expected=%q", got, want)
			}
			verRaw, _ := rawMap["verification"].(map[string]any)
			if got, want := res.Tier, asString(verRaw["type"]); got != want {
				t.Errorf("verification.type: loader=%q schema-raw=%q", got, want)
			}

			// Status must be one of the verifier's documented outputs;
			// "rejected" here would mean the verifier disagrees with
			// the schema on what's well-formed — the F31 oracle hit.
			switch res.Status {
			case "verified", "tier_unsupported":
				// fine
			case "rejected":
				t.Fatalf("verifier rejected schema-valid entry %s; reasons=%v", tc.path, res.Reasons)
			default:
				t.Fatalf("unexpected verifier status %q for %s", res.Status, tc.path)
			}
		})
	}
}

// yamlBytesToJSONBytes parses YAML and emits canonical JSON bytes. The
// jsonschema/v5 compiler and its Validate path both expect JSON-native
// values (string-keyed maps, slices, JSON primitives); routing through
// json.Marshal guarantees that shape for any yaml.v3 input.
func yamlBytesToJSONBytes(data []byte) ([]byte, error) {
	var node any
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	normalized, err := normalizeYAMLForJSON(node)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// normalizeYAMLForJSON turns a yaml.v3-decoded value into something
// json.Marshal accepts: map[interface{}]interface{} → map[string]interface{}
// with stringified keys, slices recurse, scalars pass through.
//
// yaml.v3 normally produces map[string]interface{} for object decode,
// but document-level edge cases (anchors, tagged !!map nodes, integer
// keys) can leak interface{} keys; this is a defensive guard.
func normalizeYAMLForJSON(v any) (any, error) {
	switch m := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			nv, err := normalizeYAMLForJSON(val)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, errors.New("non-string map key in YAML; not representable as JSON object")
			}
			nv, err := normalizeYAMLForJSON(val)
			if err != nil {
				return nil, err
			}
			out[ks] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(m))
		for i, val := range m {
			nv, err := normalizeYAMLForJSON(val)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	default:
		return v, nil
	}
}

// asString coerces a schema-raw value into its string form for
// loader/raw cross-checks. Non-strings produce empty so callers see a
// clean mismatch in the test failure rather than a panic.
func asString(v any) string {
	s, _ := v.(string)
	return s
}
