# Claude example guidance

Keep this the smallest readable Anthropic tool-calling example. Demonstrate
provider construction, typed tool parameters, bounded execution, and useful
error handling without introducing orchestration or UI concerns.

Validate with `go build ./...`; a live run requires `ANTHROPIC_API_KEY` and is
optional unless the task explicitly requests integration testing.
