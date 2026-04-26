package verify

// registeredPrimitives mirrors the enum on
// schema/entry.schema.yaml verification.primitives_required.items.enum
// (lines 412-441 as of 2026-04-26). When the schema's enum changes, this
// list must be updated to match — the schema is the source of truth.
var registeredPrimitives = map[string]struct{}{
	"sha1":                       {},
	"sha256":                     {},
	"sha512":                     {},
	"md5":                        {},
	"base64":                     {},
	"base64url":                  {},
	"hex":                        {},
	"utf8":                       {},
	"json_canonical":             {},
	"yaml_load_1_1":              {},
	"yaml_load_1_2":              {},
	"add":                        {},
	"sub":                        {},
	"mul":                        {},
	"div":                        {},
	"mod":                        {},
	"equal":                      {},
	"not_equal":                  {},
	"lt":                         {},
	"gt":                         {},
	"lte":                        {},
	"gte":                        {},
	"logic.and":                  {},
	"logic.or":                   {},
	"logic.not":                  {},
	"logic.any":                  {},
	"logic.all":                  {},
	"string.matches_regex":       {},
	"string.matches_glob_arn":    {},
	"k8s.parse_quantity_to_milli": {},
}

// IsRegisteredPrimitive returns true when name is in the schema-defined
// primitives set.
func IsRegisteredPrimitive(name string) bool {
	_, ok := registeredPrimitives[name]
	return ok
}
