module github.com/runlog-org/runlog-verifier

go 1.22

require (
	github.com/runlog-org/runlog-schema v0.0.0-00010101000000-000000000000
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/santhosh-tekuri/jsonschema/v5 v5.3.1

// Drop this replace once runlog-schema is tagged + published; bump the require above to the real version.
replace github.com/runlog-org/runlog-schema => ../runlog-schema
