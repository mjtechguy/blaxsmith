---
spec_format_version: v2.1
feature: frozen-recipe-example
---

# Example: frozen recipe inputs

## Problem Statement

- Recipe compilation must retain reproducible inputs and the final human gate. [from A-001, A-002]

## Scope

- Demonstrate compilation without launching workers. [from A-003]

## Functional Requirements

### Locked

- **FR-001** [from A-001]: "Freeze recipe inputs at an exact Git commit."
- **FR-002** [from A-002]: "Keep human review as the final stage."

## Global Invariants

- **GI-001** [from A-001]: "Freeze recipe inputs at an exact Git commit."

| ID | statement | applies-to | violation | citation |
| --- | --- | --- | --- | --- |
| GI-001 | Freeze recipe inputs at an exact Git commit. | compiler | mutable input | [from A-001] |

## State Transitions

| ID | from-state | to-state | trigger | guard | citation |
| --- | --- | --- | --- | --- | --- |
| ST-000 | - | - | None — compilation example has no application state transitions | - | [from A-003] |

## Contracts

| ID | surface | input | output | errors | citation |
| --- | --- | --- | --- | --- | --- |
| CT-000 | None — compilation example has no external service contracts | - | - | - | [from A-003] |

## Appendix: Interview Transcript

# Example interview (synthetic test data)

## A-001 [ARCH_INVARIANT]
Freeze recipe inputs at an exact Git commit.

## A-002
Keep human review as the final stage.

## A-003 [IMPLICIT_FACT:RUNTIME]
This example only compiles a recipe; it does not execute workers. No application state transitions or external service contracts are needed for this example.
