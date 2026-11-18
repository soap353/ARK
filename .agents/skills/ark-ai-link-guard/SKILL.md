---
name: ark-ai-link-guard
description: >-
  Use when modifying or troubleshooting Ark's Ask AI (/ask), RAG retrieval,
  inline Markdown/KaTeX citations, or ZIM viewer (?ark_hl=) links to keep
  the AI focused and guarantee every generated link resolves locally.
---

# Ark AI Link & Focus Guardrail

Use this runbook whenever changing Ask AI prompts, RAG search, or citation links in Ark.

## 1. Keep Ask AI Focused
- Inspect `handleAPIAIChat` in `internal/web/handlers_phase2.go` and `SearchKnowledge` in `internal/ai/indexer.go`.
- Keep retrieved RAG context tight (`topK = 3`), filter noisy stop words in `buildFTSQueries`, and instruct the model to answer only the user's question concisely without adding trailing `Sources:` / `References:` sections.

## 2. Guarantee Separated JSON References & Working Numbered Links
- Pass numbered context entries (`[1] Title: ...`, `[2] Title: ...`) to the model without raw URLs so the model cannot hallucinate paths.
- Instruct the model to respond in JSON (`{"title": "...", "answer": "...", "references": [{"id": 1, "quote": "..."}]}`), keeping `"title"` (max 4 words) based strictly on the user's query and keeping `"answer"` free of inline links.
- Run `parseStructuredAIResponse` (`repairJSONLaTeXEscapes` + `stripInlineLinksAndCiteBlocks` + `buildCitationLinkWithSnippet`) before returning JSON from `/api/ai/chat`:
  1. Preserve LaTeX macros (`\frac`, `\theta`, `\nabla`, `\mathbf`, etc.) and Markdown inside `"answer"`, while stripping any accidental inline links or trailing `Sources:` sections.
  2. Map each referenced `"id"` and `"quote"` to verified `/read/zim/<stem>/C/<article>?ark_hl=<quote>` or `/media/...` URLs via `resolveZIMArticleURL`.
  3. Render references as small numbered badge links (`[1]`, `[2]`) beneath the AI response (`renderNumberedReferences` in `internal/web/templates/ask.html`) with hover tooltips and `target="_blank"`.

## 3. Verify End-to-End
- Run `go vet ./...` and `go test -count=1 ./...`.
- Confirm tests verify both valid ZIM resolution and stripping of broken/external links.
