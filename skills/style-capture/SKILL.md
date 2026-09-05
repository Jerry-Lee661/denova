---
name: style-capture
description: Use when the user wants to capture an author's writing style from sample text into a reusable style reference, so future chapters imitate that voice.
category: writing
capabilities:
  - writing-workflow
agent: ide
---

# Style Capture

Extract a concrete, reusable style profile from reference text and save it as a `style_reference` so tellers and writing workflows can imitate the voice. Capture voice, not content: the output must never copy the source's plot, characters, or distinctive expressions wholesale.

## Gather the source

1. Obtain reference text via `read` (file path in the workspace) or from the user's message. Aim for at least three representative passages; one short excerpt is not enough to separate voice from accident.
2. If the sample is too short or too homogeneous, say so and ask for more before analyzing.

## Extract the profile

Analyze and record, each dimension backed by a short quoted example from the source:

- **Sentence rhythm**: typical length distribution, how long and short sentences alternate, paragraph length and shape.
- **Narration distance**: POV and tense, how close the camera sits to the character, interiority vs observation ratio.
- **Description density**: which senses dominate, how often description interrupts action, concrete-vs-abstract balance.
- **Dialogue**: share of dialogue, tag habits, subtext level, how distinct speakers sound.
- **Vocabulary register**: common/high-brow/colloquial mix, idioms, signature constructions, what the author never does.
- **Punctuation and formatting**: dash/ellipsis/semicolon habits, onomatopoeia, paragraph breaks in dialogue.
- **Pacing markers**: how scene transitions are handled, how time skips are signaled, chapter ending style.

## Draft, confirm, save

1. Write the profile as Markdown with a short title and the dimensions above. Quote sparingly: one example per dimension, at most one sentence each. State negative rules ("avoids similes", "never head-hops") — they imitate better than positive description alone.
2. Show the draft to the user and ask for confirmation and a reference name before saving.
3. Check for name collisions with `config_read` on the `style_reference` resource, then create it:

```text
config_apply({
  "operation": "create",
  "resource": "style_reference",
  "scope": "user",
  "value": {
    "name": "<user-confirmed name>",
    "description": "<one-line summary, <=240 chars>",
    "content": "<the full profile markdown>"
  }
})
```

Create fails if the derived filename already exists; on collision, propose a different name instead of updating an existing reference the user did not ask to change.

## Applying the captured style

Tell the user how to use it: reference the returned `display_path` in a narrative style's `style_refs`, or paste the profile into a writing request. Imitation quality improves when the profile is combined with their own revision pass, not used as a single-shot generator.
