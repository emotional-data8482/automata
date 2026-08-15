module github.com/emotional-data8482/automata/examples/claude

go 1.26.2

require (
	github.com/emotional-data8482/automata v0.2.1
	github.com/emotional-data8482/automata/extensions/claude v0.2.1
)

require (
	github.com/anthropics/anthropic-sdk-go v1.21.0 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/sync v0.20.0 // indirect
)

replace (
	github.com/emotional-data8482/automata => ../..
	github.com/emotional-data8482/automata/extensions/claude => ../../extensions/claude
)
