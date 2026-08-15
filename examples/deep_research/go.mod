module github.com/emotional-data8482/automata/examples/deep_research

go 1.26.2

require (
	github.com/charmbracelet/bubbles v0.20.0
	github.com/charmbracelet/bubbletea v1.2.4
	github.com/charmbracelet/lipgloss v1.0.0
	github.com/emotional-data8482/automata v0.2.1
	github.com/emotional-data8482/automata/extensions/claude v0.2.1
	github.com/emotional-data8482/automata/extensions/tavily v0.2.1
	github.com/emotional-data8482/automata/tools v0.2.1
	github.com/joho/godotenv v1.5.1
)

require (
	github.com/anthropics/anthropic-sdk-go v1.21.0 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/charmbracelet/x/ansi v0.4.5 // indirect
	github.com/charmbracelet/x/term v0.2.1 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/termenv v0.15.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.34.0 // indirect
	golang.org/x/text v0.27.0 // indirect
)

replace (
	github.com/emotional-data8482/automata => ../..
	github.com/emotional-data8482/automata/extensions/claude => ../../extensions/claude
	github.com/emotional-data8482/automata/extensions/tavily => ../../extensions/tavily
	github.com/emotional-data8482/automata/tools => ../../tools
)
