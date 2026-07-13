module github.com/emotional-data/automata/examples/openai

go 1.26.2

require (
	github.com/emotional-data/automata v0.1.0
	github.com/emotional-data/automata/extensions/openai v0.1.0
)

require golang.org/x/sync v0.20.0 // indirect

replace (
	github.com/emotional-data/automata => ../..
	github.com/emotional-data/automata/extensions/openai => ../../extensions/openai
)
