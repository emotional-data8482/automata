module github.com/emotional-data8482/automata/examples/browser

go 1.26.2

require (
	github.com/emotional-data8482/automata v0.6.0
	github.com/emotional-data8482/automata/extensions/browser v0.0.0
	github.com/emotional-data8482/automata/extensions/openrouter v0.5.2
)

require (
	github.com/OpenRouterTeam/go-sdk v0.8.21 // indirect
	github.com/go-rod/rod v0.116.2 // indirect
	github.com/spyzhov/ajson v0.8.0 // indirect
	github.com/ysmood/fetchup v0.2.3 // indirect
	github.com/ysmood/goob v0.4.0 // indirect
	github.com/ysmood/got v0.40.0 // indirect
	github.com/ysmood/gson v0.7.3 // indirect
	github.com/ysmood/leakless v0.9.0 // indirect
)

replace (
	github.com/emotional-data8482/automata => ../..
	github.com/emotional-data8482/automata/extensions/browser => ../../extensions/browser
	github.com/emotional-data8482/automata/extensions/openrouter => ../../extensions/openrouter
)
