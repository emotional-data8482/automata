# OpenAI example guidance

Keep this the smallest readable OpenAI-compatible tool-calling example. Make
the model and base URL configuration visible, keep credentials in environment
variables, and avoid endpoint-specific assumptions in shared code.

Validate with `go build ./...`; a live run requires provider credentials and is
optional unless the task explicitly requests integration testing.
