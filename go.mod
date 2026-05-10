module github.com/runlog-org/runlog-verifier

go 1.22

require (
	github.com/runlog-org/runlog-schema v0.4.1
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/santhosh-tekuri/jsonschema/v5 v5.3.1

// Local sibling clone — consumes the unreleased fixture_sidecar_process
// arm landed on hv/f82-fixture-sidecar-schema. Remove this directive
// and bump the v0.4.1 require to the new tag once runlog-schema ships
// v0.5.0 (tracked as the F82 follow-up in TODO.md).
replace github.com/runlog-org/runlog-schema => ../runlog-schema
