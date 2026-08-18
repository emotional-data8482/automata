module github.com/emotional-data8482/automata/examples/openai

go 1.26.2

require (
	github.com/emotional-data8482/automata v0.2.1
	github.com/emotional-data8482/automata/extensions/openai v0.2.1
)

replace (
	github.com/emotional-data8482/automata => ../..
	github.com/emotional-data8482/automata/extensions/openai => ../../extensions/openai
)
