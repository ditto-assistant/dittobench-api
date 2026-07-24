module github.com/ditto-assistant/dittobench-api

go 1.23

require (
	github.com/ditto-assistant/dittobench-datagen v0.11.2-0.20260722063340-ef3af0387b46
	github.com/google/uuid v1.6.0
	github.com/smacker/go-tree-sitter v0.0.0-20240827094217-dd81d9e9be82
)

// TEMPORARY: swap for tagged release before merge
replace github.com/ditto-assistant/dittobench-datagen => ../datagen-v7-hard
