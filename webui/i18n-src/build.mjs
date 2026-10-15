#!/usr/bin/env node
// i18n build pipeline for opencode2api WebUI.
// Modes:
//   node build.mjs extract   -> scans index.html + app.js, emits map.json, dupes.json, nested.json
//   node build.mjs rewrite   -> rewrites index.html + app.js using en.json/vi.json + map.json
//   node build.mjs assemble  -> validates coverage, emits ../i18n.js
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.dirname(fileURLToPath(import.meta.url));
const WEBUI = path.resolve(SRC, "..");
const CJK = /[㐀-䶿一-鿿぀-ヿ豈-￯、-〜！-～]/;
const hasCJK = (s) => CJK.test(s);
const norm = (s) => s.replace(/\s+/g, " ").trim();

const BACKEND = [
  "已有 Key %d",
  "名称需为 1–80 个字符",
  "自定义 Key 需为 16–256 位",
  "Key 仅支持英文字母、数字、短横线、下划线和点",
  "最多创建 200 个 API Key",
  "此 API Key 已存在",
  "无法生成密钥",
  "API Key 不存在，请刷新列表",
  "至少保留一个启用的 API Key，请先创建替代密钥",
  "至少保留一个启用的 API Key",
  "OpenCode 返回的额度格式无法识别",
  "OpenCode 返回的额度窗口不完整",
  "额度查询地址无效",
  "暂时无法连接 OpenCode，请稍后刷新",
  "Go API Key 无效或已被撤销",
  "此 Key 未获准读取 Go 套餐额度",
  "额度查询过于频繁，请稍后刷新",
  "OpenCode 额度接口返回 HTTP %d",
  "OpenCode 返回套餐共享额度，未提供逐模型独立余额；多个 Go Key 可能属于同一套餐，不能相加。百分比由上游取整。",
  "模型列表已更新，请刷新后重新排序",
  "只能调整组内模型的顺序",
  "首个有效输出超时需为 1–300 秒",
  "两个分组的别名不能相同",
  "别名需为 1–64 位英文字母、数字、点、短横线或下划线",
  "别名不能与真实模型 ID 相同",
  "模型顺序包含重复、空值或不属于该分组的模型",
  "SOTA 组必须包含指定的五个模型",
  "额度不足",
  "配额已用尽",
  "该分组暂无可用模型（已暂停、下架、额度不足或重试次数已用尽），请稍后重试",
  "分组请求整体超时",
  "当前上游模型的输出中断；本次响应已结束，请重新发起请求。",
  "当前模型暂停或已下架",
  "请选择该组当前可用的 Go 模型",
  "管理员手动切换",
  "轮转状态保存失败",
  "轮转状态写入失败，请检查磁盘空间和文件权限",
];
const backendKey = (s) => {
  let i = 0;
  return norm(s).replace(/%d/g, () => `{${i++}}`);
};

// ---- JS tokenizer: yields string literals with positions ----
function* jsStrings(code) {
  const n = code.length;
  let i = 0;
  const lineOf = (p) => code.slice(0, p).split("\n").length;
  while (i < n) {
    const c = code[i];
    if (c === "/" && code[i + 1] === "/") {
      while (i < n && code[i] !== "\n") i++;
      continue;
    }
    if (c === "/" && code[i + 1] === "*") {
      i += 2;
      while (i < n && !(code[i] === "*" && code[i + 1] === "/")) i++;
      i += 2;
      continue;
    }
    if (c === "'" || c === '"' || c === "`") {
      const q = c, start = i;
      let raw = c;
      i++;
      let depth = 0;
      let closed = false;
      while (i < n) {
        const ch = code[i];
        if (ch === "\\") { raw += ch + (code[i + 1] ?? ""); i += 2; continue; }
        if (q === "`" && ch === "$" && code[i + 1] === "{") {
          depth++;
          raw += "${"; i += 2; continue;
        }
        if (q === "`" && depth > 0 && ch === "{") { depth++; raw += ch; i++; continue; }
        if (q === "`" && depth > 0 && ch === "}") { depth--; raw += ch; i++; continue; }
        if (ch === q && depth === 0) { raw += ch; i++; closed = true; break; }
        // a backtick/quote inside ${...} nested string: let it be consumed as part of expr
        raw += ch; i++;
      }
      if (closed) yield { raw, start, end: i, line: lineOf(start), quote: q };
      continue;
    }
    i++;
  }
}

function unescapeJS(body, q) {
  // body without surrounding quotes; handles standard escapes
  return body.replace(/\\(\\|n|t|r|'|"|`|\$)/g, (m, e) => {
    switch (e) {
      case "n": return "\n";
      case "t": return "\t";
      case "r": return "\r";
      default: return e;
    }
  });
}

// split template raw into text parts and ${expr} parts (top-level only)
function splitTemplate(raw) {
  const body = raw.slice(1, -1);
  const parts = [];
  let i = 0, cur = "";
  while (i < body.length) {
    if (body[i] === "\\") { cur += body[i] + (body[i + 1] ?? ""); i += 2; continue; }
    if (body[i] === "$" && body[i + 1] === "{") {
      parts.push({ text: cur }); cur = "";
      let d = 1; i += 2; let expr = "";
      while (i < body.length && d > 0) {
        if (body[i] === "\\") { expr += body[i] + (body[i + 1] ?? ""); i += 2; continue; }
        // skip nested strings inside expr
        if (body[i] === "'" || body[i] === '"' || body[i] === "`") {
          const qq = body[i]; expr += qq; i++;
          while (i < body.length) {
            if (body[i] === "\\") { expr += body[i] + (body[i + 1] ?? ""); i += 2; continue; }
            expr += body[i];
            if (body[i] === qq) { i++; break; }
            i++;
          }
          continue;
        }
        if (body[i] === "{") d++;
        if (body[i] === "}") { d--; if (d === 0) { i++; break; } }
        expr += body[i]; i++;
      }
      parts.push({ expr });
      continue;
    }
    cur += body[i]; i++;
  }
  parts.push({ text: cur });
  return parts;
}

function jsKeyAndArgs(raw, q) {
  if (q === "`") {
    const parts = splitTemplate(raw);
    let idx = 0;
    const keyParts = [], args = [];
    for (const p of parts) {
      if (p.expr !== undefined) { keyParts.push(`{${idx++}}`); args.push(p.expr.trim()); }
      else keyParts.push(unescapeJS(p.text, q));
    }
    return { key: norm(keyParts.join("")), args };
  }
  return { key: norm(unescapeJS(raw.slice(1, -1), q)), args: [] };
}

function extractAll() {
  const html = fs.readFileSync(path.join(WEBUI, "index.html"), "utf8");
  const js = fs.readFileSync(path.join(WEBUI, "app.js"), "utf8");
  const items = []; // {kind, zh, file, line, ctx, extra}
  const lineOf = (src, p) => src.slice(0, p).split("\n").length;

  const reText = />([^<>]+)</g;
  let m;
  while ((m = reText.exec(html))) {
    if (!hasCJK(m[1])) continue;
    const t = norm(m[1]);
    if (!t) continue;
    items.push({ kind: "html-text", zh: t, file: "index.html", line: lineOf(html, m.index),
      ctx: html.slice(Math.max(0, m.index - 60), m.index + 80).replace(/\s+/g, " ") });
  }
  const reAttr = /(placeholder|aria-label|title)="([^"]*)"/g;
  while ((m = reAttr.exec(html))) {
    if (!hasCJK(m[2])) continue;
    items.push({ kind: "html-attr:" + m[1], zh: norm(m[2]), file: "index.html", line: lineOf(html, m.index),
      ctx: html.slice(Math.max(0, m.index - 60), m.index + 80).replace(/\s+/g, " ") });
  }
  const nested = [];
  for (const lit of jsStrings(js)) {
    const content = lit.quote === "`" ? lit.raw.slice(1, -1) : lit.raw.slice(1, -1);
    if (!hasCJK(content)) continue;
    // check CJK inside ${...}
    if (lit.quote === "`") {
      const parts = splitTemplate(lit.raw);
      const innerCJK = parts.some((p) => p.expr !== undefined && hasCJK(p.expr));
      if (innerCJK) {
        // still register inner literals individually below via separate scan of exprs
        nested.push({ raw: lit.raw.slice(0, 160), line: lit.line, start: lit.start, end: lit.end });
      }
    }
    const { key } = jsKeyAndArgs(lit.raw, lit.quote);
    if (!key) continue;
    items.push({ kind: "js", zh: key, file: "app.js", line: lit.line,
      ctx: js.slice(Math.max(0, lit.start - 60), lit.end + 40).replace(/\s+/g, " "),
      start: lit.start, end: lit.end, raw: lit.raw, quote: lit.quote, nested: lit.quote === "`" && nested.some((x) => x.start === lit.start) });
  }
  // inner literals of nested templates: scan expr parts
  for (const nst of nested) {
    const full = js.slice(nst.start, nst.end);
    for (const lit of jsStrings(full)) {
      if (lit.start === 0) continue; // the outer itself
      const inner = lit.raw;
      if (!hasCJK(inner)) continue;
      const { key } = jsKeyAndArgs(inner, lit.quote);
      if (!key) continue;
      items.push({ kind: "js-inner", zh: key, file: "app.js", line: nst.line, ctx: ("NESTED-IN: " + nst.raw.slice(0, 120)).replace(/\s+/g, " ") });
    }
  }
  for (const b of BACKEND) items.push({ kind: "backend", zh: backendKey(b), file: "(backend)", line: 0, ctx: b });
  return { items, nested };
}

function buildMap(items) {
  const map = {}; // id -> zh
  const rev = {}; // zh -> id
  const dupes = {}; // zh -> [items]
  let counter = 1;
  for (const it of items) {
    if (rev[it.zh]) {
      (dupes[it.zh] ||= []).push(it);
      continue;
    }
    const id = "t" + String(counter++).padStart(3, "0");
    rev[it.zh] = id;
    map[id] = it.zh;
    dupes[it.zh] = [it];
  }
  return { map, rev, dupes };
}

const mode = process.argv[2];
if (mode === "extract") {
  const { items, nested } = extractAll();
  const { map, dupes } = buildMap(items);
  fs.writeFileSync(path.join(SRC, "map.json"), JSON.stringify(map, null, 1));
  const dupeList = Object.entries(dupes).filter(([, v]) => v.length > 1).map(([zh, v]) => ({ zh, count: v.length, uses: v.map((x) => `${x.file}:${x.line} [${x.kind}] ${x.ctx.slice(0, 140)}`) }));
  fs.writeFileSync(path.join(SRC, "dupes.json"), JSON.stringify(dupeList, null, 1));
  fs.writeFileSync(path.join(SRC, "nested.json"), JSON.stringify(nested.map((x) => ({ line: x.line, raw: x.raw.slice(0, 200) })), null, 1));
  console.log("TOTAL ITEMS:", items.length, "UNIQUE:", Object.keys(map).length);
  console.log("DUPES:", dupeList.length, "NESTED-SKIP:", nested.length);
  for (const d of dupeList) {
    console.log("\n--- DUPE x" + d.count + ": [" + d.zh.slice(0, 90) + "]");
    d.uses.forEach((u) => console.log("    " + u.slice(0, 200)));
  }
  console.log("\n--- NESTED (manual handling):");
  nested.forEach((x) => console.log(`  app.js:${x.line} ${x.raw.slice(0, 160)}`));
} else if (mode === "rewrite") {
  const map = JSON.parse(fs.readFileSync(path.join(SRC, "map.json"), "utf8"));
  const en = JSON.parse(fs.readFileSync(path.join(SRC, "en.json"), "utf8"));
  const rev = {};
  for (const [id, zh] of Object.entries(map)) rev[zh] = id;
  const missing = Object.keys(map).filter((id) => !en[id]);
  if (missing.length) { console.error("en.json MISSING:", missing.join(",")); process.exit(1); }
  const escAttr = (s) => s.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;");
  const escHtml = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

  // ---- index.html ----
  let html = fs.readFileSync(path.join(WEBUI, "index.html"), "utf8");
  html = html.replace('<script src="/app.js" defer></script>', '<script src="/i18n.js"></script>\n    <script src="/app.js" defer></script>');
  html = html.replace(/<html lang="zh-CN">/, '<html lang="en">');
  // Language switcher: static markup lives in the toolbar (app.js hook below
  // re-renders it on language change via window.o2aLangChanged).
  html = html.replace(
    '<button id="refresh-now" class="ghost">',
    '<select id="lang-switch" class="ghost lang-select" aria-label="Language">' +
      '<option value="en">English</option>' +
      '<option value="zh">\u4e2d\u6587</option>' +
      '<option value="vi">Ti\u1ebfng Vi\u1ec7t</option>' +
      '</select><button id="refresh-now" class="ghost">',
  );
  // Also offer the switcher on the login screen (no toolbar there).
  html = html.replace(
    '<button class="primary wide">',
    '<div class="field"><label for="login-lang">Language / \u8bed\u8a00 / Ng\u00f4n ng\u1eef</label>' +
      '<select id="login-lang" class="lang-select">' +
      '<option value="en">English</option>' +
      '<option value="zh">\u4e2d\u6587</option>' +
      '<option value="vi">Ti\u1ebfng Vi\u1ec7t</option>' +
      '</select></div><button class="primary wide">',
  );
  html = html.replace(/<([a-zA-Z][a-zA-Z0-9]*)((?:\s[^<>]*?)?)>([^<>]*[\u4e00-\u9fff\u3000-\u303F\uFF00-\uFFEF][^<>]*)</g, (full, tag, attrs, txt) => {
    if (/data-i18n=/.test(attrs)) return full;
    const t = norm(txt);
    const id = rev[t];
    if (!id) { console.error("NO ID for html text:", t.slice(0, 80)); return full; }
    if (/data-i18n=/.test(full)) return full;
    return `<${tag}${attrs} data-i18n="${id}">${escHtml(en[id])}<`;
  });
  html = html.replace(/(placeholder|aria-label|title)="([^"]*)"/g, (full, attr, val) => {
    if (!hasCJK(val)) return full;
    const id = rev[norm(val)];
    if (!id) { console.error("NO ID for attr:", val.slice(0, 80)); return full; }
    const key = attr === "placeholder" ? "data-i18n-ph" : attr === "aria-label" ? "data-i18n-aria" : "data-i18n-title";
    return `${attr}="${escAttr(en[id])}" ${key}="${id}"`;
  });
  fs.writeFileSync(path.join(WEBUI, "index.html"), html);
  console.log("index.html rewritten");

  // ---- app.js ----
  let js = fs.readFileSync(path.join(WEBUI, "app.js"), "utf8");
  // collect replacements (skip nested outer templates: handled manually)
  const { nested } = extractAll();
  const nestedStarts = new Set(nested.map((x) => x.start));
  const reps = [];
  for (const lit of jsStrings(js)) {
    const content = lit.raw.slice(1, -1);
    if (!hasCJK(content)) continue;
    if (nestedStarts.has(lit.start)) continue; // manual
    const { key, args } = jsKeyAndArgs(lit.raw, lit.quote);
    const id = rev[key];
    if (!id) { console.error("NO ID for js:", key.slice(0, 80)); continue; }
    reps.push({ start: lit.start, end: lit.end, text: args.length ? `t("${id}", ${args.join(", ")})` : `t("${id}")` });
  }
  reps.sort((a, b) => b.start - a.start);
  for (const r of reps) js = js.slice(0, r.start) + r.text + js.slice(r.end);
  // locale-aware date/number formatting
  js = js.replace(/\.toLocaleString\("zh-CN", \{ hour12: false \}\)/, ".toLocaleString(LOC(), { hour12: false })");
  js = js.replace(/\.toLocaleString\(\)/g, ".toLocaleString(LOC())");
  js = js.replace(/\.toLocaleTimeString\(\)/g, ".toLocaleTimeString(LOC())");
  fs.writeFileSync(path.join(WEBUI, "app.js"), js);
  console.log("app.js rewritten, literals replaced:", reps.length);
} else if (mode === "assemble") {
  const map = JSON.parse(fs.readFileSync(path.join(SRC, "map.json"), "utf8"));
  const en = JSON.parse(fs.readFileSync(path.join(SRC, "en.json"), "utf8"));
  const vi = JSON.parse(fs.readFileSync(path.join(SRC, "vi.json"), "utf8"));
  const ids = Object.keys(map);
  const bad = [];
  for (const id of ids) {
    if (!en[id]) bad.push("en:" + id);
    if (!vi[id]) bad.push("vi:" + id);
  }
  const extraEn = Object.keys(en).filter((k) => !map[k]);
  const extraVi = Object.keys(vi).filter((k) => !map[k]);
  if (bad.length || extraEn.length || extraVi.length) {
    console.error("COVERAGE FAIL:", JSON.stringify({ bad: bad.slice(0, 20), extraEn, extraVi }));
    process.exit(1);
  }
  const core = fs.readFileSync(path.join(SRC, "core.js"), "utf8");
  const out = core
    .replace("/*__DICT_EN__*/", JSON.stringify(en))
    .replace("/*__DICT_ZH__*/", JSON.stringify(map))
    .replace("/*__DICT_VI__*/", JSON.stringify(vi));
  fs.writeFileSync(path.join(WEBUI, "i18n.js"), out);
  // verify: every data-i18n / t("...") id exists
  const html = fs.readFileSync(path.join(WEBUI, "index.html"), "utf8");
  const js = fs.readFileSync(path.join(WEBUI, "app.js"), "utf8");
  const used = new Set();
  for (const m of html.matchAll(/data-i18n(?:-ph|-aria|-title)?="(t\d+)"/g)) used.add(m[1]);
  for (const m of js.matchAll(/\bt\("(t\d+)"\)/g)) used.add(m[1]);
  const unknown = [...used].filter((k) => !map[k]);
  const unused = ids.filter((k) => !used.has(k));
  console.log("used ids:", used.size, "unknown:", JSON.stringify(unknown));
  console.log("UNUSED (backend/tx/manual/nested):", unused.join(","));
  // leftover CJK outside i18n.js zh dict / tx table
  const stripSpans = html.replace(/<span data-i18n="t\d+">.*?<\/span>/g, "");
  const leftHtml = [...stripSpans.matchAll(/[^<>]*[㐀-䶿一-鿿぀-ヿ豈-￯、-〜！-～][^<>]*/g)].map((m) => m[0].trim()).filter(Boolean);
  console.log("leftover CJK in index.html:", JSON.stringify(leftHtml.slice(0, 10)));
  const jsNoT = js.replace(/\bt\("t\d+"[^)]*\)/g, "");
  const leftJs = [];
  for (const lit of jsStrings(jsNoT)) if (hasCJK(lit.raw)) leftJs.push(lit.raw.slice(0, 100));
  console.log("leftover CJK literals in app.js:", leftJs.length, JSON.stringify(leftJs.slice(0, 10)));
} else {
  console.error("usage: node build.mjs extract|rewrite|assemble");
  process.exit(1);
}
