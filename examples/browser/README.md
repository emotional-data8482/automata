# Browser + OpenRouter example

From `examples/browser`, with Chrome or Chromium installed:

```sh
export OPENROUTER_API_KEY='your-openrouter-key'
go run .
```

This makes live, billable OpenRouter calls. The default model is
`openai/gpt-5.2`; select another tool-capable model with `OPENROUTER_MODEL`.
If Chrome is not detected, set `AUTOMATA_BROWSER_PATH` to its executable.

## What to watch for

- A visible browser opens English Wikipedia. The agent chooses a topic, types
  its query, opens an article, and follows an internal link to a related article.
- The terminal prints the model, task, and browser tool calls.
- The final answer should include the actual query, both visited article titles
  and URLs, and three observed facts attributed to their articles. Compare these
  with the browser while it runs; the browser closes when the run ends.
- `wikipedia.org`, `www.wikipedia.org`, and `en.wikipedia.org` are pre-approved.
  Any other host requires a terminal `y`/`n` approval. Only approve hosts you trust.

## CAPTCHAs

If a page shows a CAPTCHA or a similar challenge, the agent first tries to
solve it itself when the challenge is simple text it can read from a
screenshot, typing the answer with `browser_act`. Image puzzles such as
reCAPTCHA grids are deliberately adversarial and cannot be answered reliably
that way, so for those the agent calls `browser_checkpoint`: the run pauses
with a terminal `y`/`n` prompt, you solve the challenge in the visible browser
window, and answering `y` resumes the run. Denying tells the agent to continue
without the page.

This needs the visible browser (the default). `-headed=false` refuses
`-checkpoint` — a CAPTCHA cannot be solved in a browser nobody can see. The
agent is never told to bypass a challenge or invent facts.

## Options

```sh
# Pick your own search task.
go run . "Visit https://en.wikipedia.org and use its search field to find the Go programming language. Open the article and report three facts with its URL."

# Run without a visible window.
go run . -headed=false

# Try a simpler page.
go run . -allow example.com "Open https://example.com and report the page title and visible text."

# Disable the CAPTCHA checkpoint (implies -headed=false, or fails).
go run . -headed=false -checkpoint=false

# Give a checkpoint-heavy run more time.
go run . -minutes 20
```

The run has a ten-minute deadline (`-minutes`), 50 model turns, and 40 tool
requests. Browser actions execute sequentially; a pending checkpoint also
consumes run time, so raise `-minutes` when you expect to solve one. Failed
runs report partial output.

## Offline validation

```sh
go build ./...
go test ./...
```

These checks do not use credentials, open a browser, or call OpenRouter. To test
actual browsing, use `go run .` interactively as above.
