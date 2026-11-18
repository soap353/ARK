# Ark Project Agent Focus & Link Verification Rules

## 1. Stay Strictly On-Task
- Keep changes minimal, direct, and scoped strictly to the user's request.
- Reuse existing packages, database helpers, and stdlib features rather than adding parallel layers or speculative abstractions.
- Never silently swallow offline failures; surface accurate status in UI and logs.

## 2. Offline-First & Working Links Only
- Ark runs completely offline: never introduce runtime CDN script/style URLs (`cdn.jsdelivr.net`, `unpkg.com`, etc.) or external web links in generated UI/AI content.
- Before claiming any link, route, ZIM article URL, or citation works, verify the target route or handler exists and returns a non-404 status (or resolves cleanly against local `data/zim/`, `data/media/`, or registered mux routes).
- For Ark's Ask AI (`/ask`), validate every generated Markdown link against retrieved sources, local ZIM archives, or registered Ark routes, and strip dead/hallucinated links down to plain text rather than serving broken links.
