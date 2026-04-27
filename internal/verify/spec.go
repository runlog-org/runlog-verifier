package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// matchOutcome compares one branch's ExecResult against the relevant
// differential keys. Returns the (possibly empty) list of mismatch reasons.
func matchOutcome(branch string, got runner.ExecResult, diff map[string]any, retKey, raiseKey string) []Reason {
	retSpec, hasRet := diff[retKey]
	raiseSpec, hasRaise := diff[raiseKey]

	if !hasRet && !hasRaise {
		// No expectation declared for this branch — accept silently.
		// failed_branch_must_fail_with is a separate spec key, deferred.
		return nil
	}

	if got.Raised {
		if !hasRaise {
			return []Reason{{
				Code: "unexpected_exception",
				Message: fmt.Sprintf("%s raised %s (%s) but spec required a return value",
					branch, got.Exception, got.Message),
			}}
		}
		wantTypes, wantPattern, err := exceptionFromSpec(raiseSpec)
		if err != nil {
			return []Reason{{Code: "malformed_raise_spec", Message: err.Error()}}
		}
		if len(wantTypes) > 0 {
			matched := false
			for _, t := range wantTypes {
				if t == got.Exception {
					matched = true
					break
				}
			}
			if !matched {
				return []Reason{{
					Code: "wrong_exception",
					Message: fmt.Sprintf("%s raised %s, spec required %s",
						branch, got.Exception, formatExpectedExceptions(wantTypes)),
				}}
			}
		}
		if wantPattern != "" {
			re, err := regexp.Compile(wantPattern)
			if err != nil {
				return []Reason{{
					Code:    "malformed_raise_spec",
					Message: fmt.Sprintf("message_pattern %q is not a valid regex: %v", wantPattern, err),
				}}
			}
			if !re.MatchString(got.Message) {
				return []Reason{{
					Code: "wrong_exception_message",
					Message: fmt.Sprintf("%s raised %s with message %q, spec required message matching /%s/",
						branch, got.Exception, got.Message, wantPattern),
				}}
			}
		}
		return nil
	}

	if !hasRet {
		return []Reason{{
			Code: "unexpected_return",
			Message: fmt.Sprintf("%s returned %s value, spec required a raised exception",
				branch, got.TypeName),
		}}
	}
	return matchReturnSpec(branch, got, retSpec)
}

// matchReturnSpec compares ExecResult against a {type, value_equals} spec.
// The schema's open-ended `failed_branch_must_return: {}` allows other
// shapes; v0.1 supports {type, value_equals} and rejects others with a
// distinct reason so submitters know to wait for the next slice.
func matchReturnSpec(branch string, got runner.ExecResult, spec any) []Reason {
	sm, ok := spec.(map[string]any)
	if !ok {
		return []Reason{{
			Code: "unsupported_return_spec",
			Message: fmt.Sprintf("%s_branch_must_return: only {type, value_equals} shape is supported in v0.1, got %T",
				branch, spec),
		}}
	}

	var out []Reason

	if specType, ok := sm["type"].(string); ok && specType != got.TypeName {
		out = append(out, Reason{
			Code: "wrong_return_type",
			Message: fmt.Sprintf("%s returned type %s, spec required %s",
				branch, got.TypeName, specType),
		})
	}

	if val, hasVal := sm["value_equals"]; hasVal {
		if !got.Serializable {
			out = append(out, Reason{
				Code: "non_serializable_return",
				Message: fmt.Sprintf("%s returned non-serializable value (repr=%s); cannot compare to value_equals",
					branch, got.Repr),
			})
			return out
		}
		wantBytes, err := json.Marshal(val)
		if err != nil {
			out = append(out, Reason{
				Code:    "malformed_value_equals",
				Message: fmt.Sprintf("%s_branch_must_return.value_equals: %v", branch, err),
			})
			return out
		}
		gotCanonical, err := canonicalizeJSON(got.JSONValue)
		if err != nil {
			out = append(out, Reason{
				Code:    "branch_runner_error",
				Message: fmt.Sprintf("%s: canonicalize return value: %v", branch, err),
			})
			return out
		}
		if !bytes.Equal(wantBytes, gotCanonical) {
			out = append(out, Reason{
				Code: "wrong_return_value",
				Message: fmt.Sprintf("%s returned %s, spec required %s",
					branch, string(gotCanonical), string(wantBytes)),
			})
		}
	}

	if want, ok := lengthFromSpec(sm["length"]); ok {
		if got.Length == nil {
			out = append(out, Reason{
				Code: "non_sized_return",
				Message: fmt.Sprintf("%s returned %s value (no len()); cannot compare to length=%d",
					branch, got.TypeName, want),
			})
		} else if *got.Length != want {
			out = append(out, Reason{
				Code: "wrong_return_length",
				Message: fmt.Sprintf("%s returned length=%d, spec required length=%d",
					branch, *got.Length, want),
			})
		}
	}

	if elemType, ok := sm["contains_exception_type"].(string); ok {
		if len(got.ElementTypes) == 0 {
			out = append(out, Reason{
				Code: "non_iterable_return",
				Message: fmt.Sprintf("%s returned %s value (no element types captured); cannot check contains_exception_type=%s",
					branch, got.TypeName, elemType),
			})
		} else {
			found := false
			for _, t := range got.ElementTypes {
				if t == elemType {
					found = true
					break
				}
			}
			if !found {
				out = append(out, Reason{
					Code: "missing_exception_element",
					Message: fmt.Sprintf("%s returned elements %v, spec required at least one of type %s",
						branch, got.ElementTypes, elemType),
				})
			}
		}
	}

	return out
}

// lengthFromSpec extracts an integer length from a YAML-decoded value. yaml.v3
// decodes plain integers as int (Go), but as the exact YAML int kind. Accept
// both int and float64 (defensive — JSON-decoded values come through as float64).
func lengthFromSpec(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		// Reject non-integer floats.
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// canonicalizeJSON re-marshals raw through Go's encoder so that comparisons
// don't fail on whitespace differences between Python's json.dumps and
// Go's encoding/json (e.g., Python emits "[1, 2]", Go emits "[1,2]").
func canonicalizeJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// exceptionFromSpec extracts the expected exception class names and optional
// message-pattern regex from a *_must_raise spec. Accepts:
//   - nil → no constraints
//   - bare string → single class name, no message pattern
//   - {type|class|exception: <name>, message_pattern: <regex>} → single class + pattern
//   - {exception_any: [<name>, ...], message_pattern: <regex>} → any-of + pattern
//
// Returns (nil, "", nil) when the spec is nil. Returns (typeNames, pattern, nil)
// otherwise. Empty typeNames means "any exception class is acceptable" — the
// pattern alone constrains.
func exceptionFromSpec(spec any) (typeNames []string, messagePattern string, err error) {
	switch v := spec.(type) {
	case nil:
		return nil, "", nil
	case string:
		return []string{v}, "", nil
	case map[string]any:
		// exception_any takes precedence over single-name keys.
		if anyVal, ok := v["exception_any"]; ok {
			list, listOK := anyVal.([]any)
			if !listOK {
				return nil, "", fmt.Errorf("exception_any must be a list of class names")
			}
			for _, item := range list {
				s, sOK := item.(string)
				if !sOK {
					return nil, "", fmt.Errorf("exception_any entry %v is not a string", item)
				}
				typeNames = append(typeNames, s)
			}
		} else {
			// Single-name keys, in priority order: exception, type, class.
			for _, key := range []string{"exception", "type", "class"} {
				if s, ok := v[key].(string); ok {
					typeNames = []string{s}
					break
				}
			}
		}
		if mp, ok := v["message_pattern"].(string); ok {
			messagePattern = mp
		}
		if len(typeNames) == 0 && messagePattern == "" {
			return nil, "", errors.New("raise spec object missing exception/type/class/exception_any field")
		}
		return typeNames, messagePattern, nil
	default:
		return nil, "", fmt.Errorf("raise spec has unsupported shape %T", spec)
	}
}

// formatExpectedExceptions renders one or more expected exception class names
// for the Reason message — single name as `Foo`, multiple as `any of [A, B]`.
func formatExpectedExceptions(types []string) string {
	if len(types) == 1 {
		return types[0]
	}
	return fmt.Sprintf("any of %v", types)
}
