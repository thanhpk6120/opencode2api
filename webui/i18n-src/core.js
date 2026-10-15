"use strict";
/* opencode2api WebUI i18n runtime. Default language: English ("en").
 * Dictionaries are injected at build time by i18n-src/build.mjs (assemble).
 * ZH doubles as the lookup table that translates backend (Chinese) error
 * messages on the client via tx(), so no backend change is required. */
const I18N_DICTS = {
  en: /*__DICT_EN__*/,
  zh: /*__DICT_ZH__*/,
  vi: /*__DICT_VI__*/,
};
const I18N_LOCALES = { en: "en-US", zh: "zh-CN", vi: "vi-VN" };
const I18N_HTML_LANG = { en: "en", zh: "zh-CN", vi: "vi" };
const I18N_DEFAULT = "en";
const I18N_KEY = "o2a-lang";

let LANG = I18N_DEFAULT;
try {
  const saved = window.localStorage.getItem(I18N_KEY);
  if (saved && I18N_DICTS[saved]) LANG = saved;
} catch (_) {
  /* storage unavailable: stay on default */
}

// Reverse table: Chinese source string -> id (for tx()).
const ZH2ID = {};
for (const [id, zh] of Object.entries(I18N_DICTS.zh || {})) {
  if (ZH2ID[zh] === undefined) ZH2ID[zh] = id;
}

function fmt(template, args) {
  return String(template).replace(/\{(\d+)\}/g, (m, n) => {
    const v = args[Number(n)];
    return v === undefined || v === null ? m : String(v);
  });
}

// Translate by id, e.g. t("t012") or t("t220", a, b, c).
// Unknown ids pass through unchanged (e.g. language-neutral literals).
function t(id, ...args) {
  const d = I18N_DICTS[LANG] || {};
  const fb = I18N_DICTS[I18N_DEFAULT] || {};
  const s = d[id] !== undefined ? d[id] : fb[id] !== undefined ? fb[id] : id;
  return args.length ? fmt(s, args) : s;
}

// Translate a free-form message (backend error text). Exact Chinese match
// first, then a digit-normalized match ("已有 Key 5" -> "已有 Key {0}").
function tx(msg) {
  if (typeof msg !== "string" || !msg) return msg;
  let id = ZH2ID[msg];
  if (id !== undefined) return t(id);
  let i = 0;
  const pat = msg.replace(/\d+(?:\.\d+)?/g, () => `{${i++}}`);
  id = ZH2ID[pat];
  if (id !== undefined) {
    const nums = msg.match(/\d+(?:\.\d+)?/g) || [];
    return t(id, ...nums);
  }
  return msg;
}

function LOC() {
  return I18N_LOCALES[LANG] || I18N_LOCALES[I18N_DEFAULT];
}

function applyStatic() {
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    el.textContent = t(el.getAttribute("data-i18n"));
  });
  document.querySelectorAll("[data-i18n-ph]").forEach((el) => {
    el.setAttribute("placeholder", t(el.getAttribute("data-i18n-ph")));
  });
  document.querySelectorAll("[data-i18n-aria]").forEach((el) => {
    el.setAttribute("aria-label", t(el.getAttribute("data-i18n-aria")));
  });
  document.querySelectorAll("[data-i18n-title]").forEach((el) => {
    el.setAttribute("title", t(el.getAttribute("data-i18n-title")));
  });
  document.querySelectorAll("select.lang-select").forEach((sel) => {
    sel.value = LANG;
  });
}

function setLang(lang) {
  if (!I18N_DICTS[lang]) return false;
  LANG = lang;
  try {
    window.localStorage.setItem(I18N_KEY, lang);
  } catch (_) {
    /* storage unavailable */
  }
  document.documentElement.lang = I18N_HTML_LANG[lang] || lang;
  applyStatic();
  if (typeof window.o2aLangChanged === "function") {
    try {
      window.o2aLangChanged(lang);
    } catch (_) {
      /* dynamic re-render is best-effort */
    }
  }
  return true;
}

function currentLang() {
  return LANG;
}

// Public handle for app.js and inline handlers.
window.o2aI18n = { t, tx, setLang, currentLang, LOC };

// Set <html lang> as early as possible; apply static text once the DOM is
// ready, so a saved non-English preference renders before async paints.
document.documentElement.lang = I18N_HTML_LANG[LANG] || LANG;
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", applyStatic, { once: true });
} else {
  applyStatic();
}
