---
name: de-ai-flavor
description: Use when the user asks to remove AI flavor from prose, humanize AI-generated text, or audit chapters for template AI patterns before publication.
category: writing
capabilities:
  - writing-workflow
agent: ide
---

# De-AI Flavor

Rewrite AI-flavored prose so it reads like a human author wrote it, while preserving every plot fact, name, number, and promise already on the page. This skill audits first and rewrites second; never rewrite a passage you have not diagnosed.

## Scope

1. Ask for or infer the target range: a scene, a chapter, or a chapter range.
2. Read the target with `read`. For long chapters, work in sequential windows and keep a running list of findings.

## Detect before rewriting

Scan for these families of AI tells and record line references:

- **Stock imagery and gestures**: formulaic micro-gestures reused as filler beats — glinting eyes, a curling mouth corner, frozen air, an unnoticed smile. Search the target language's common phrasings for each family (in Chinese webnovels these are fixed four-to-eight character templates; in English, "a glint passed through", "something shifted in their eyes", "he let out a breath he didn't know he was holding").
- **AI-vocabulary tells**: register words that correlate with machine drafting. In Chinese: hedging frameworks ("it's worth noting that", "rather than saying... it's better to say") used more than once per scene. In English: "delve", "tapestry", "testament to", "palpable", "couldn't help but", "a mix of X and Y".
- **Rhythm monotony**: paragraphs of near-identical length, every paragraph three sentences, sentences that always end on the stressed abstract noun.
- **Over-parallelism**: triplet lists in almost every paragraph; "not X but Y" constructions (and their Chinese equivalents) stacked back to back.
- **Narrated emotion**: feelings announced (he felt anger, warmth rose in her heart) instead of shown through action, sensation, or dialogue.
- **Over-explanation**: the text explains a motivation the reader already inferred, or restates the previous beat in new words.
- **Polished-voice dialogue**: every character speaks in complete, balanced sentences with no interruption, dialect, or subtext; dialogue tags limited to one said-variant.

## Rewrite rules

- Cut or replace flagged stock phrases with a concrete, scene-specific detail. Prefer one specific noun over two vague adjectives.
- Vary rhythm deliberately: follow any long sentence with a short one. Let important beats land in a paragraph of a single line.
- Convert announced emotion into behavior: an action beat, a physical sensation, or a change in what the character notices.
- Break parallelism: keep at most one triplet per scene, and only when the content earns it.
- Trim hedges and restatements. If a sentence only repeats the previous one, delete it.
- Give at least one character in each dialogue scene a speech habit (clipped, run-on, deflecting) and let tags vary or disappear.
- Respect the established narrative style: if a style reference or teller defines voice rules, they override generic suggestions from this skill.

## Verify

1. Re-read the rewritten range once. Confirm plot facts, timeline, character knowledge, and foreshadowing are unchanged.
2. Check drift: total length should stay within roughly ±10% of the original unless the user asked for heavier surgery.
3. Report to the user: what patterns were found (with counts), what was changed, and any passage you deliberately left alone because it was fine.

Do not run this skill on text the user has not asked you to change; detection without a rewrite request should be reported as findings only.
