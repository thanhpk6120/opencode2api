# WebUI i18n (EN default · ZH · VI)

Default language: **English**. The switcher (toolbar + login screen)
offers English / 中文 / Tiếng Việt and persists to
`localStorage["o2a-lang"]`.

## Files

| File | Role |
|---|---|
| `webui/i18n.js` | Generated runtime + 3 dictionaries (do not hand-edit) |
| `webui/i18n-src/core.js` | Runtime: `t()`, `tx()`, `setLang()`, `LOC()`, `window.o2aI18n` |
| `webui/i18n-src/map.json` | id (`t001`–`t433`) → Chinese source |
| `webui/i18n-src/en.json` | English (default) |
| `webui/i18n-src/vi.json` | Vietnamese |
| `webui/i18n-src/build.mjs` | Pipeline: `extract` / `rewrite` / `assemble` |
| `webui/i18n-src/dupes.json`, `nested.json` | Extract diagnostics |

`zh` is not a separate file: `map.json` doubles as the Chinese table,
so backend (Chinese) error strings translate client-side via `tx()`
without touching Go code.

## Usage in code

- Static HTML: `data-i18n="t012"`, `data-i18n-ph="t204"`,
  `data-i18n-aria="…"`, `data-i18n-title="…"` (element text only —
  never put `data-i18n` on a node that also holds `<input>`,
  `<select>`, `<code>` or counter `<span>` children; wrap the text
  part in an inner `<span>` instead).
- Dynamic JS: `t("t012")`, `t("t220", a, b)` (`{0}` placeholders).
- Backend/free-form messages: `tx(msg)` (exact zh match, then
  digit-normalized match; otherwise passthrough).
- Locale-aware dates/numbers: `LOC()` → `toLocaleString(LOC())`.
- Language change: `window.o2aI18n.setLang("zh")`; dynamic pages
  re-render through `window.o2aLangChanged` (defined in `app.js`).
- `const pages` in `app.js` is a lazy `Proxy` over `pageMeta()` so
  nav titles follow the active language.

## Rebuild after editing strings

```bash
node webui/i18n-src/build.mjs assemble   # validates en/vi coverage, emits ../i18n.js
node --check webui/i18n.js && node --check webui/app.js
```

`extract` re-scans sources (regenerates `map.json` — only for new
strings; keep existing ids stable). `rewrite` is one-shot and already
applied; do not re-run it on the translated tree.
