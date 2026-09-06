---
name: write-projects
description: Break down feature ideas into implementation-ready project plans, user stories, and technical specs. Use when turning vague product/feature requests into well-documented work that agents or developers can implement autonomously.
---

# Write Projects

Use this skill to turn a feature idea into a project package that is clear enough for autonomous implementation. The output should preserve product intent, expose assumptions, define acceptance criteria, and split the work into independently verifiable implementation slices.

## Core Principles

- Start from the user's desired outcome, not from an assumed implementation.
- Inspect the repository before finalizing technical plans; match existing architecture, conventions, and terminology.
- Prefer small, testable slices over broad tasks.
- Make requirements explicit: goals, non-goals, constraints, assumptions, risks, dependencies, and open questions.
- Write documentation that another agent can pick up later without needing the original conversation.
- Do not over-specify internals when uncertainty remains. Mark open questions instead.
- Keep implementation plans autonomous: every task should include enough context, file/module hints, expected behavior, and validation steps.

## Workflow

### 1. Intake the Feature Idea

Capture:

- Problem or opportunity
- Target users/personas
- Desired outcome
- Existing pain points
- Constraints or deadlines
- Known non-goals
- Success metrics, if provided

If the request is vague, ask only the minimum clarifying questions needed to avoid planning the wrong thing. If progress is still possible, proceed with stated assumptions.

### 2. Inspect Project Context

Before writing technical specs, inspect relevant repo context:

- `README.md`, docs, planning files, and roadmap files
- Existing feature/module layout
- Related tests and examples
- Public APIs, CLI commands, or UI surfaces affected by the feature
- Existing naming, style, and architecture patterns

Use `project_report`, `symbol_search`, `module_report`, `read`, and `rg` as appropriate. Do not invent architecture that conflicts with the repo.

### 3. Create a Project Package

Write project docs under:

```text
planning/projects/<project-slug>/
```

Recommended files:

```text
planning/projects/<project-slug>/
├── README.md                # project brief and navigation
├── user-stories.md          # user stories and acceptance criteria
├── specs.md                 # product + technical specification
├── implementation-plan.md   # autonomous implementation tasks
└── validation.md            # test, review, and rollout checklist
```

For very small features, a single `planning/projects/<project-slug>.md` is acceptable, but prefer the directory structure when there are multiple stories or implementation phases.

### 4. Write the Project Brief

`README.md` should include:

```markdown
# <Project Name>

## Summary

<One-paragraph description of the feature and why it matters.>

## Goals

- ...

## Non-Goals

- ...

## Users / Personas

- ...

## Success Criteria

- ...

## Scope

### In Scope

- ...

### Out of Scope

- ...

## Key Decisions

- ...

## Open Questions

- ...

## Documents

- [User Stories](user-stories.md)
- [Specs](specs.md)
- [Implementation Plan](implementation-plan.md)
- [Validation](validation.md)
```

### 5. Write User Stories

Use outcome-focused stories:

```markdown
## Story <N>: <Title>

As a <persona>, I want <capability>, so that <benefit>.

### Acceptance Criteria

- Given <context>, when <action>, then <observable result>.
- ...

### Notes

- Dependencies:
- Edge cases:
- Related specs/tasks:
```

Guidelines:

- Each story should be independently understandable.
- Acceptance criteria must be observable and testable.
- Include edge cases and error states.
- Link stories to implementation tasks where useful.

### 6. Write Specs

`specs.md` should combine product and technical requirements.

Suggested sections:

```markdown
# Specs

## Functional Requirements

- ...

## Technical Requirements

- ...

## Data Model / Interfaces

- ...

## UX / API Behavior

- ...

## Error Handling

- ...

## Security / Privacy / Safety

- ...

## Performance / Reliability

- ...

## Compatibility and Migration

- ...

## Observability

- Logs:
- Metrics:
- Tracing:

## Alternatives Considered

- ...
```

Adapt sections to the project. For library/API features, emphasize API surface, backwards compatibility, examples, and tests. For UI features, emphasize states, flows, accessibility, and copy.

### 7. Write Autonomous Implementation Tasks

`implementation-plan.md` should break work into tasks that can be implemented by an agent in order.

Use this format:

```markdown
## Task <N>: <Imperative Title>

### Objective

<What this task accomplishes.>

### Context

<Relevant files, modules, existing patterns, and dependencies.>

### Steps

1. ...
2. ...
3. ...

### Acceptance Criteria

- ...

### Validation

- `go test ./...`
- <targeted tests/checks>

### Notes / Risks

- ...
```

Task rules:

- One task should have a clear end state.
- Include likely files to inspect or edit, but avoid pretending certainty if unknown.
- Include tests or validation for every task.
- Identify dependencies between tasks.
- Avoid bundling unrelated refactors with feature work.

### 8. Write Validation and Rollout Plan

`validation.md` should include:

```markdown
# Validation

## Automated Checks

- ...

## Manual Checks

- ...

## Regression Risks

- ...

## Rollout / Migration

- ...

## Definition of Done

- [ ] User stories acceptance criteria satisfied
- [ ] Specs implemented or explicitly deferred
- [ ] Tests added/updated
- [ ] Docs/examples updated
- [ ] Backwards compatibility reviewed
- [ ] Open questions resolved or tracked
```

## Quality Checklist

Before finishing, verify the project package has:

- A concise summary and clear goals/non-goals
- User stories with testable acceptance criteria
- Specs grounded in the repository's actual architecture
- Implementation tasks small enough for autonomous execution
- Explicit validation commands and expected checks
- Risks, assumptions, dependencies, and open questions
- Links between project docs where useful

## Response Style

When creating or updating project docs, respond with:

1. Files created or changed
2. Brief summary of the project structure
3. Any unresolved questions or assumptions

Keep the response concise; the documentation should carry the detail.
