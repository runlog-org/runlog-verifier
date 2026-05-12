package verify

import (
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/cassette"
)

func TestIsShareStateSafe(t *testing.T) {
	tests := []struct {
		name      string
		setup     []string
		mutations []Mutation
		want      bool
	}{
		{
			name:      "empty setup_script + no mutations -> safe",
			setup:     nil,
			mutations: nil,
			want:      true,
		},
		{
			name:      "empty setup_script + mutate_fixture -> safe (no setup state to leak)",
			setup:     nil,
			mutations: []Mutation{{Strategy: "mutate_fixture", Target: "$SOURCE_PATH"}},
			want:      true,
		},
		{
			name:      "empty setup_script + setup-targeted drop_flag -> safe (no setup to scan)",
			setup:     nil,
			mutations: []Mutation{{Strategy: "drop_flag", Target: "working_approach.setup.dockerfile"}},
			want:      true,
		},
		{
			name:      "non-empty setup + no mutations -> safe",
			setup:     []string{"echo hi"},
			mutations: nil,
			want:      true,
		},
		{
			name:      "non-empty setup + only set_literal_value -> safe",
			setup:     []string{"echo hi"},
			mutations: []Mutation{{Strategy: "set_literal_value", Target: "$LITERAL_1"}},
			want:      true,
		},
		{
			name:      "non-empty setup + swap_function_call on action -> safe",
			setup:     []string{"echo hi"},
			mutations: []Mutation{{Strategy: "swap_function_call", Target: "failed_approach.action.python_code"}},
			want:      true,
		},
		{
			name:      "non-empty setup + mutate_fixture -> unsafe",
			setup:     []string{"echo hi"},
			mutations: []Mutation{{Strategy: "mutate_fixture", Target: "$SOURCE_PATH"}},
			want:      false,
		},
		{
			name:      "non-empty setup + setup-targeted drop_flag -> unsafe",
			setup:     []string{"echo hi"},
			mutations: []Mutation{{Strategy: "drop_flag", Target: "working_approach.setup.dockerfile"}},
			want:      false,
		},
		{
			name:      "non-empty setup + setup-targeted remove_kwarg -> unsafe",
			setup:     []string{"echo hi"},
			mutations: []Mutation{{Strategy: "remove_kwarg", Target: "failed_approach.setup.shell_kwarg"}},
			want:      false,
		},
		{
			name:      "non-empty setup + mixed safe+unsafe -> unsafe (any unsafe taints)",
			setup:     []string{"echo hi"},
			mutations: []Mutation{
				{Strategy: "set_literal_value", Target: "$LITERAL_1"},
				{Strategy: "mutate_fixture", Target: "$SOURCE_PATH"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cas := &cassette.Cassette{SetupScript: tt.setup}
			got := IsShareStateSafe(cas, tt.mutations)
			if got != tt.want {
				t.Errorf("IsShareStateSafe() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsShareStateSafe_NilCassette(t *testing.T) {
	if IsShareStateSafe(nil, nil) != false {
		t.Errorf("IsShareStateSafe(nil, nil) = true, want false (defensive nil-guard)")
	}
	if IsShareStateSafe(nil, []Mutation{{Strategy: "set_literal_value"}}) != false {
		t.Errorf("IsShareStateSafe(nil, mutations) = true, want false (defensive nil-guard)")
	}
}
