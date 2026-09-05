---
name: multi-perspective-review
description: Use when the user asks for a structured review or critique of finished chapters from several distinct perspectives, such as reader, editor, and peer author.
category: writing
capabilities:
  - writing-workflow
agent: ide
---

# Multi-Perspective Review

Produce a structured critique of the target chapters from three independent perspectives, then synthesize one prioritized action list. This skill reviews; it does not rewrite. Fixes happen only after the user picks items.

## Scope

1. Confirm the target: chapter, chapter range, or a scene. Read the target with `read` and, when relevant, the surrounding outline (`setting/outline.md`) and character states (`setting/character-states.md`) so continuity claims are grounded.
2. Do not summarize the plot back to the user at length; they wrote it.

## The three passes

Run each perspective as a separate pass. When SubAgent delegation is available, delegate each pass to a SubAgent with the rubric below and its own fresh reading; otherwise perform them sequentially, deliberately switching rubric between passes. Never blend perspectives inside one pass.

**Pass 1 — Reader.** Optimize for the experience of someone who pays for the next chapter.
- Hook: does the opening line/scene create a question or tension strong enough to continue?
- Comprehension: any point where a first-time reader loses the thread of who/where/when?
- Pacing: scenes that drag (cut candidates) or rush past a beat that earned space.
- Emotional payoff: does the chapter deliver at least one moment that lands? Where does it deflate?

**Pass 2 — Editor.** Optimize for structural integrity and commercial viability.
- Continuity: contradictions with the outline, character states, or earlier chapters; name, geography, and timeline errors.
- Stakes: are consequences real and tracked? Any promised foreshadowing with no visible anchor?
- Scene purpose: for each scene, one line stating what changes; scenes where nothing changes are cut-or-merge candidates.
- Market fit: chapter ending strength, length relative to platform norms, title/blurb promises kept.

**Pass 3 — Peer author.** Optimize for craft, peer-to-peer, blunt but specific.
- Prose: repeated sentence rhythms, weak verbs, filter words (perception verbs that narrate what a character sees or feels instead of the thing itself), stock phrases.
- Dialogue: does each speaker have distinct voice? Tags and beats varied? Subtext present?
- Show vs tell: passages that narrate conclusions instead of dramatizing them.
- Technique choices: POV consistency, tense consistency, info-dump detection.

## Synthesis and output

1. Merge findings into one list. Tag each item: `blocker` (breaks story logic or loses the reader), `major` (clearly weakens the chapter), `polish` (local improvement).
2. Reference every finding by chapter and quoted fragment, never by line number alone. Quote at most one sentence per finding.
3. End with a prioritized top-5 action list, each item phrased as a concrete change ("Cut the dream sequence in ch.12", not "improve pacing").
4. Present the report and stop. Offer to execute the accepted items, and route prose-level fixes through the appropriate writing skill afterwards.
