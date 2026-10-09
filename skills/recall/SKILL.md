---
name: recall
description: Locate and verify existing book content before reading, writing, or updating. Use when you must recall where a scene, fact, rule, or passage appears; gather material by topic; check the outline, creative rules, progress, ideas, setting files, or character state; look up the lore library; or check for duplicates before creating lore, characters, locations, or items.
category: writing
agent: ide,general
---

# recall

Retrieve before you write. Fragments returned by search are pointers; a fact becomes authoritative only after `read` returns its source.

## When to load

- Locating where something appears: which chapter, where a fact was established, when two characters first met.
- Recalling content not in current context: earlier scenes, past decisions, previous state.
- Gathering material by topic across the workspace before planning, rewriting, or answering.
- Verifying a fact, rule, or established detail before relying on it.
- Creating or updating lore, characters, locations, factions, or items: check for duplicates first.

Do not load it for content already read this turn or already present in context.

## Read order by content type

| Content | Read directly | Use `search` when |
| --- | --- | --- |
| `setting/outline.md` (plan) | In full before planning or when structure matters | Only to find which chapter or plan section covers a topic |
| `CREATOR.md` (rules) | In full; it is already injected every turn | Never; fragments must not substitute for a rule |
| `setting/progress.md` (progress) | In full before continuing prose | Only to recall which chapter covered an event |
| `ideas.md` (direction) | In full when direction or premises matter | To find earlier discussion fragments |
| `setting/character-states.md` (state) | In full before writing a scene with that character | To trace historical state changes |
| Other `setting/` files | When the exact file is known | To aggregate facts spread across files |
| `chapters/` prose | The specific chapter a pointer names | To locate passages by meaning, quote, or topic |
| Lore library | `list_lore_items` to filter, `read_lore_items` for full bodies | Not covered by workspace `search`; keep using Lore tools |
| Imported reference material (rulebooks, style files) | Only the matched sections after searching | Primary use: search first, then read the match |

## Discipline

1. Locate with `search` (or `grep` for an exact known string), then read the located source before relying on, quoting, or editing it.
2. Keep the source set minimal; do not re-read files that are already current in context this turn.
3. If `search` is unavailable, fall back to `grep` and direct `read`.
4. Before creating a new lore item, character, location, or item, look for an existing one with `list_lore_items` keywords or `search`, and update the existing entity instead of duplicating it.
5. When an answer depends on retrieved material, cite the workspace-relative paths you relied on.
