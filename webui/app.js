const $ = (selector) => document.querySelector(selector),
  $$ = (selector) => [...document.querySelectorAll(selector)];
let csrf = "",
  config = null,
  monitorData = null,
  debugData = null,
  lastPlayground = null,
  logEvents = [],
  eventSource = null,
  paused = false,
  monitorTimer = null,
  logRenderPending = false,
  toastTimer = null;
function pageMeta() {
  return {
    proxies: ["13", t("t434"), t("t435")],
    availability: ["14", t("t018"), t("t207")],
    rotation: ["12", t("t017"), t("t208")],
    overview: ["01", t("t007"), t("t019")],
    guide: ["02", t("t008"), t("t209")],
    access: ["03", t("t009"), t("t210")],
    usage: ["04", t("t010"), t("t211")],
    playground: ["05", "Playground", t("t212")],
    diagnostics: ["06", t("t011"), t("t213")],
    config: ["07", t("t012"), t("t214")],
    logs: ["08", t("t013"), t("t215")],
    account: ["09", t("t014"), t("t216")],
    "model-quotas": ["10", t("t015"), t("t217")],
    "api-keys": ["11", t("t016"), t("t218")],
  };
}
const pages = new Proxy(
  {},
  {
    get: (_, prop) => pageMeta()[prop],
    has: (_, prop) => prop in pageMeta(),
  },
);
async function api(path, options = {}) {
  options.headers = { ...(options.headers || {}), Accept: "application/json" };
  if (options.body) options.headers["Content-Type"] = "application/json";
  if (csrf && !["GET", "HEAD"].includes(options.method || "GET"))
    options.headers["X-CSRF-Token"] = csrf;
  const response = await fetch(path, options);
  if (response.status === 401) {
    showLogin();
    throw new Error(t("t219"));
  }
  if (response.status === 204) return null;
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error?.message || `HTTP ${response.status}`);
  return data;
}
function toast(message, bad = false) {
  const node = $("#toast");
  node.textContent = message;
  node.classList.toggle("bad-toast", bad);
  node.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => node.classList.add("hidden"), 3600);
}
function showLogin() {
  csrf = "";
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  if (eventSource) eventSource.close();
  eventSource = null;
  clearInterval(monitorTimer);
  clearInterval(quotaTimer);
  clearCreatedKey();
  $("#api-key-value").value = "";
}
async function enterConsole(session) {
  csrf = session.csrf_token;
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  await loadConfig();
  await Promise.all([refreshMonitor(), loadDebugModels()]);
  clearInterval(monitorTimer);
  monitorTimer = setInterval(refreshMonitor, 3000);
  connectLogs();
  openPage(pages[location.hash.slice(1)] ? location.hash.slice(1) : "overview");
}
async function boot() {
  try {
    await enterConsole(await api("/api/auth/session"));
  } catch (_) {
    showLogin();
  }
}
$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("#login-error").textContent = "";
  try {
    const session = await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username: $("#login-user").value, password: $("#login-pass").value }),
    });
    $("#login-pass").value = "";
    await enterConsole(session);
  } catch (error) {
    $("#login-error").textContent = error.message;
  }
});
$("#logout").onclick = async () => {
  try {
    await api("/api/auth/logout", { method: "POST", body: "{}" });
  } finally {
    showLogin();
  }
};
function openPage(page) {
  if (!pages[page]) return;
  history.replaceState(null, "", `#${page}`);
  clearInterval(quotaTimer);
  if (page !== "api-keys") clearCreatedKey();
  $$(".nav button").forEach((button) =>
    button.classList.toggle("active", button.dataset.page === page),
  );
  $$(".page").forEach((node) => node.classList.toggle("hidden", node.id !== `page-${page}`));
  const meta = pages[page];
  $("#page-index").textContent = `Section ${meta[0]}`;
  $("#page-title").textContent = meta[1];
  $("#page-sub").textContent = meta[2];
  if (page === "access") renderAccessExamples();
  if (page === "usage") renderUsage();
  if (page === "model-quotas") {
    loadModelQuotas();
    quotaTimer = setInterval(() => {
      if (!document.hidden) loadModelQuotas();
    }, 60000);
  }
  if (page === "rotation") loadRotation();
  if (page === "proxies") loadProxies();
  if (page === "availability") loadAvailability();
  if (page === "api-keys") loadAPIKeys();
  if (page === "playground" && !debugData) loadDebugModels();
  if (page === "diagnostics") {
    loadDebugModels();
    renderDiagnostics();
  }
}
$$(".nav button").forEach((button) => (button.onclick = () => openPage(button.dataset.page)));
async function loadConfig() {
  config = await api("/api/config");
  fillConfig(config);
  renderAccessExamples();
}
function fillConfig(value) {
  $("#c-listen").value = value.listen;
  $("#c-prefer").value = value.prefer;
  $("#c-web-listen").value = value.webui.listen;
  $("#c-session").value = value.webui.session_ttl_minutes;
  $("#c-web-enabled").checked = value.webui.enabled;
  $("#c-anonymous").checked = !!value.anonymous;
  $("#c-proxyfile").value = value.proxyfile;
  $("#c-up-zen").value = value.upstream.zen;
  $("#c-up-go").value = value.upstream.go;
  $("#c-attempts").value = value.retry.max_attempts;
  $("#c-timeout").value = value.retry.timeout_seconds;
  $("#c-refresh").value = value.models.refresh_seconds;
  $("#c-protocols").value = JSON.stringify(value.models.protocols || {}, null, 2);
  $("#c-free-only").checked = !!value.models.free_only;
  const reasoning = value.reasoning || {};
  $("#c-reasoning-effort").value = reasoning.effort || "";
  $("#c-reasoning-by-model").value = JSON.stringify(reasoning.effort_by_model || {}, null, 2);
  $("#c-idle").value = value.performance.max_idle_conns;
  $("#c-idle-host").value = value.performance.max_idle_conns_per_host;
  $("#c-max-host").value = value.performance.max_conns_per_host;
  $("#c-idle-timeout").value = value.performance.idle_conn_timeout_seconds;
  $("#c-connect").value = value.performance.connect_timeout_seconds;
  $("#c-cooldown").value = value.performance.failure_cooldown_seconds;
  $("#c-attempt-timeout").value = value.performance.attempt_timeout_seconds;
  $("#c-first-event-timeout").value = value.performance.first_event_timeout_seconds || 0;
  $("#c-level").value = value.logging.level;
  $("#c-ring").value = value.logging.ring_size;
  $("#c-dump-bodies").checked = !!value.logging.dump_request_bodies;
  ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(renderChips);
  const notice = $("#restart-notice");
  if (value.restart_required_fields?.length) {
    notice.textContent = t("t220", value.restart_required_fields.join(", "), value.effective.listen, value.effective.webui_listen);
    notice.classList.remove("hidden");
  } else notice.classList.add("hidden");
}
function renderChips(name) {
  const host = $(`#${name}-chips`);
  host.replaceChildren();
  (config[name] || []).forEach((item) => {
    const chip = document.createElement("span"),
      text = document.createElement("span"),
      remove = document.createElement("button");
    chip.className = "chip";
    chip.dataset.id = item.id;
    text.textContent = item.display;
    remove.type = "button";
    remove.title = t("t221");
    remove.textContent = "×";
    remove.onclick = () => chip.remove();
    chip.append(text, remove);
    host.appendChild(chip);
  });
}
function secretInputs(name) {
  const kept = [...$(`#${name}-chips`).children].map((node) => ({ id: node.dataset.id }));
  const added = $(`#${name}-new`)
    .value.split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => ({ value }));
  return kept.concat(added);
}
const number = (id) => Number($(id).value);
$("#config-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const update = {
      listen: $("#c-listen").value,
      server_keys: secretInputs("server_keys"),
      zen_keys: secretInputs("zen_keys"),
      go_keys: secretInputs("go_keys"),
      anonymous: $("#c-anonymous").checked,
      proxies: secretInputs("proxies"),
      proxyfile: $("#c-proxyfile").value,
      prefer: $("#c-prefer").value,
      upstream: { zen: $("#c-up-zen").value, go: $("#c-up-go").value },
      retry: { max_attempts: number("#c-attempts"), timeout_seconds: number("#c-timeout") },
      models: {
        refresh_seconds: number("#c-refresh"),
        protocols: JSON.parse($("#c-protocols").value || "{}"),
        free_only: $("#c-free-only").checked,
      },
      reasoning: {
        effort: $("#c-reasoning-effort").value,
        effort_by_model: JSON.parse($("#c-reasoning-by-model").value || "{}"),
      },
      performance: {
        max_idle_conns: number("#c-idle"),
        max_idle_conns_per_host: number("#c-idle-host"),
        max_conns_per_host: number("#c-max-host"),
        idle_conn_timeout_seconds: number("#c-idle-timeout"),
        connect_timeout_seconds: number("#c-connect"),
        failure_cooldown_seconds: number("#c-cooldown"),
        attempt_timeout_seconds: number("#c-attempt-timeout"),
        first_event_timeout_seconds: number("#c-first-event-timeout"),
      },
      logging: {
        level: $("#c-level").value,
        ring_size: number("#c-ring"),
        dump_request_bodies: $("#c-dump-bodies").checked,
      },
      webui: {
        enabled: $("#c-web-enabled").checked,
        listen: $("#c-web-listen").value,
        username: config.webui.username,
        session_ttl_minutes: number("#c-session"),
      },
    };
    const result = await api("/api/config", { method: "PUT", body: JSON.stringify(update) });
    config = result.config;
    ["server_keys", "zen_keys", "go_keys", "proxies"].forEach(
      (name) => ($(`#${name}-new`).value = ""),
    );
    fillConfig(config);
    await loadDebugModels();
    toast(
      result.result.restart_required
        ? t("t222", result.result.restart_fields.join(", "))
        : t("t223"),
    );
  } catch (error) {
    toast(tx(error.message), true);
  }
});
$("#reload-config").onclick = async () => {
  try {
    const result = await api("/api/config/reload", { method: "POST", body: "{}" });
    await loadConfig();
    await loadDebugModels();
    toast(result.restart_required ? t("t224") : t("t225"));
  } catch (error) {
    toast(tx(error.message), true);
  }
};
$("#reveal").onclick = async () => {
  const password = prompt(t("t226"));
  if (password === null) return;
  try {
    const values = await api("/api/config/reveal", {
      method: "POST",
      body: JSON.stringify({ password }),
    });
    $("#modal-content").textContent = JSON.stringify(values, null, 2);
    $("#modal").classList.remove("hidden");
  } catch (error) {
    toast(tx(error.message), true);
  }
};
$("#modal-close").onclick = () => {
  $("#modal").classList.add("hidden");
  $("#modal-content").textContent = "";
};
$("#account-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    await api("/api/account", {
      method: "PUT",
      body: JSON.stringify({
        current_password: $("#a-current").value,
        username: $("#a-user").value,
        new_password: $("#a-new").value,
      }),
    });
    toast(t("t227"));
    setTimeout(showLogin, 800);
  } catch (error) {
    toast(tx(error.message), true);
  }
});
function appendCell(row, value, className = "") {
  const cell = document.createElement("td");
  cell.textContent = String(value);
  if (className) cell.className = className;
  row.appendChild(cell);
}
function emptyRow(host, columns, message = t("t228")) {
  const row = document.createElement("tr"),
    cell = document.createElement("td");
  cell.colSpan = columns;
  cell.className = "empty";
  cell.textContent = message;
  row.appendChild(cell);
  host.appendChild(row);
}
function renderResources(resources) {
  const proxies = $("#proxy-table");
  proxies.replaceChildren();
  (resources.proxies || []).forEach((proxy) => {
    const row = document.createElement("tr");
    appendCell(row, proxy.address);
    appendCell(row, proxy.healthy ? t("t229") : t("t230"), `status ${proxy.healthy ? "ok" : "bad"}`);
    appendCell(
      row,
      t("t231", proxy.zen_keys, proxy.go_keys, proxy.anonymous ? t("t422") : t("t423")),
    );
    proxies.appendChild(row);
  });
  if (!proxies.children.length) emptyRow(proxies, 3);
  const keys = $("#key-table");
  keys.replaceChildren();
  if (resources.anonymous) {
    const row = document.createElement("tr");
    appendCell(row, "anonymous");
    appendCell(row, "zen");
    appendCell(row, t("t232"), "status ok");
    keys.appendChild(row);
  }
  (resources.keys || []).forEach((key) => {
    const row = document.createElement("tr");
    appendCell(row, key.id);
    appendCell(row, key.tier);
    appendCell(
      row,
      key.cooldown_until ? t("t233") : t("t234"),
      `status ${key.cooldown_until ? "warn" : "ok"}`,
    );
    keys.appendChild(row);
  });
  if (!keys.children.length) emptyRow(keys, 3);
}
function renderBreakdowns(metrics) {
  const host = $("#breakdowns");
  host.replaceChildren();
  [
    [t("t235"), metrics.endpoints],
    [t("t236"), metrics.statuses],
    [t("t025"), metrics.models],
    ["Tier", metrics.tiers],
  ].forEach(([title, data]) => {
    const block = document.createElement("div"),
      heading = document.createElement("h3"),
      table = document.createElement("table"),
      body = document.createElement("tbody");
    heading.textContent = title;
    table.className = "table";
    Object.entries(data || {})
      .sort((a, b) => b[1] - a[1])
      .slice(0, 10)
      .forEach(([key, value]) => {
        const row = document.createElement("tr");
        appendCell(row, key);
        appendCell(row, value);
        body.appendChild(row);
      });
    if (!body.children.length) emptyRow(body, 2, t("t237"));
    table.appendChild(body);
    block.append(heading, table);
    host.appendChild(block);
  });
}
function renderGuide() {
  if (!config || !monitorData) return;
  const resources = monitorData.resources,
    models = resources.models || {},
    modelDescription = models.stale
      ? t("t238") + (models.cache_source || t("t239")) + t("t240")
      : t("t241");
  const checks = [
    [t("t242"), t("t243"), true],
    [t("t244"), t("t245"), (config.server_keys || []).length > 0],
    [
      t("t246"),
      t("t247"),
      (config.zen_keys || []).length + (config.go_keys || []).length > 0 || config.anonymous,
    ],
    [
      t("t248"),
      t("t249"),
      (resources.proxies || []).some((proxy) => proxy.healthy),
    ],
    [t("t250"), modelDescription, models.exposed > 0 && !!models.updated_at],
    [t("t251"), t("t252"), true],
  ];
  const host = $("#guide-steps");
  host.replaceChildren();
  checks.forEach(([title, description, done]) => {
    const node = document.createElement("article"),
      heading = document.createElement("h3"),
      copy = document.createElement("p"),
      state = document.createElement("div");
    node.className = "guide-step";
    heading.textContent = title;
    copy.textContent = description;
    state.className = `step-state ${done ? "ok" : "warn"}`;
    state.textContent = done ? t("t253") : t("t254");
    node.append(heading, copy, state);
    host.appendChild(node);
  });
}
const formatTokens = (value) => Number(value || 0).toLocaleString(LOC());
function renderUsage() {
  if (!monitorData) return;
  const usage = monitorData.usage?.last_hour || {},
    tokens = usage.tokens || {};
  $("#u-coverage").textContent = `${((usage.coverage || 0) * 100).toFixed(1)}%`;
  $("#u-total").textContent = formatTokens(tokens.total_tokens);
  $("#u-cached").textContent = formatTokens(tokens.cached_tokens);
  $("#u-reasoning").textContent = formatTokens(tokens.reasoning_tokens);
  const trend = $("#usage-trend"),
    series = monitorData.metrics?.series || [],
    maximum = Math.max(1, ...series.map((point) => point.total_tokens || 0));
  trend.replaceChildren();
  series.forEach((point) => {
    const bar = document.createElement("div");
    bar.className = "trend-bar";
    bar.style.height = `${Math.max(2, Math.round(((point.total_tokens || 0) / maximum) * 100))}%`;
    bar.title = `${new Date(point.minute).toLocaleTimeString(LOC())} · input ${formatTokens(point.input_tokens)} · output ${formatTokens(point.output_tokens)} · total ${formatTokens(point.total_tokens)}`;
    trend.appendChild(bar);
  });
  const models = $("#usage-models");
  models.replaceChildren();
  Object.entries(usage.models || {})
    .sort((a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0))
    .slice(0, 15)
    .forEach(([model, value]) => {
      const row = document.createElement("tr");
      appendCell(row, model);
      appendCell(row, formatTokens(value.input_tokens));
      appendCell(row, formatTokens(value.output_tokens));
      appendCell(row, formatTokens(value.total_tokens));
      models.appendChild(row);
    });
  if (!models.children.length) emptyRow(models, 4, t("t255"));
  const tiers = $("#usage-tiers"),
    tierEntries = Object.entries(usage.tiers || {}).sort(
      (a, b) => (b[1].total_tokens || 0) - (a[1].total_tokens || 0),
    ),
    tierTotal = Math.max(1, ...tierEntries.map(([, value]) => value.total_tokens || 0));
  tiers.replaceChildren();
  tierEntries.forEach(([tier, value]) => {
    const row = document.createElement("div"),
      name = document.createElement("strong"),
      track = document.createElement("div"),
      fill = document.createElement("div"),
      amount = document.createElement("span");
    row.className = "tier-row";
    name.textContent = tier;
    track.className = "tier-track";
    fill.className = "tier-fill";
    fill.style.width = `${Math.round(((value.total_tokens || 0) / tierTotal) * 100)}%`;
    amount.className = "mono";
    amount.textContent = formatTokens(value.total_tokens);
    track.appendChild(fill);
    row.append(name, track, amount);
    tiers.appendChild(row);
  });
  if (!tierEntries.length) {
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.textContent = t("t256");
    tiers.appendChild(empty);
  }
}
function renderFacts(selector, entries) {
  const host = $(selector);
  host.replaceChildren();
  entries.forEach(([label, value, tone]) => {
    const fact = document.createElement("dl"),
      term = document.createElement("dt"),
      detail = document.createElement("dd");
    fact.className = "fact";
    term.textContent = label;
    detail.textContent = value ?? "—";
    if (tone) detail.className = tone;
    fact.append(term, detail);
    host.appendChild(fact);
  });
}
function renderPlayKeys() {
  const select = $("#play-key");
  if (!select) return;
  const selected = select.value;
  select.replaceChildren();
  const auto = document.createElement("option");
  auto.value = "auto";
  auto.textContent = t("t080");
  select.appendChild(auto);
  const keys = debugData?.keys || {};
  [
    ["zen", "Zen Keys"],
    ["go", "Go Keys"],
  ].forEach(([tier, label]) => {
    const items = keys[tier] || [];
    if (!items.length) return;
    const group = document.createElement("optgroup");
    group.label = label;
    items.forEach((item) => {
      const option = document.createElement("option");
      option.value = `${tier}:${item.id}`;
      const cooling = item.cooldown_until
        ? t("t257", new Date(item.cooldown_until).toLocaleTimeString(LOC()))
        : "";
      option.textContent = `${tier.toUpperCase()} · ${item.display} · ${item.id}${cooling}`;
      group.appendChild(option);
    });
    select.appendChild(group);
  });
  if (selected && [...select.options].some((option) => option.value === selected))
    select.value = selected;
  else select.value = "auto";
}
async function loadDebugModels() {
  try {
    debugData = await api("/api/debug/models");
    if (debugData.last_inference) lastPlayground = debugData.last_inference;
    const select = $("#play-model"),
      selected = select.value;
    select.replaceChildren();
    (debugData.models || []).forEach((item) => {
      const option = document.createElement("option");
      option.value = item.model;
      option.textContent = item.model;
      select.appendChild(option);
    });
    if (selected && [...select.options].some((option) => option.value === selected))
      select.value = selected;
    renderPlayKeys();
    if (!$("#play-request").value.trim()) resetPlayTemplate();
    renderDiagnostics();
  } catch (error) {
    toast(tx(error.message), true);
  }
}
function playgroundTemplate() {
  const protocol = $("#play-protocol").value,
    model = $("#play-model").value || "YOUR_MODEL";
  if (protocol === "responses") return { model, input: t("t258") };
  if (protocol === "anthropic")
    return {
      model,
      max_tokens: 512,
      messages: [{ role: "user", content: t("t258") }],
    };
  return { model, messages: [{ role: "user", content: t("t258") }] };
}
function resetPlayTemplate() {
  $("#play-request").value = JSON.stringify(playgroundTemplate(), null, 2);
}
$("#play-protocol").addEventListener("change", resetPlayTemplate);
$("#play-model").addEventListener("change", () => {
  try {
    const request = JSON.parse($("#play-request").value);
    request.model = $("#play-model").value;
    $("#play-request").value = JSON.stringify(request, null, 2);
  } catch (_) {
    resetPlayTemplate();
  }
});
$("#play-reset").onclick = resetPlayTemplate;
function displayPlayground(result) {
  lastPlayground = result;
  const keyTest = result.key_test
    ? {
        usable: t("t259"),
        rejected: t("t260"),
        rate_limited: t("t261"),
        upstream_error: t("t262"),
        transport_error: t("t263"),
        unavailable: t("t264"),
        request_error: t("t265"),
      }[result.key_test] || result.key_test
    : "";
  $("#play-state").textContent = result.ok
    ? t("t266", result.http_status)
    : t("t267", result.http_status);
  $("#play-state").className = `section-caption ${result.ok ? "ok" : "bad"}`;
  const route = result.route || {},
    eligibility = route.anonymous_eligibility || {},
    selected = result.selected_key,
    routeLabel = route.tier
      ? `${route.tier} / ${route.anonymous ? "anonymous" : route.key_id || "key"}`
      : t("t268");
  renderFacts("#play-facts", [
    [t("t269"), result.http_status, result.ok ? "ok" : "bad"],
    [t("t104"), `${result.duration_ms} ms`],
    ["Request ID", result.request_id || "—"],
    [t("t270"), selected ? t("t271", selected.tier.toUpperCase()) : t("t272")],
    [t("t273"), selected ? `${selected.display} · ${selected.id}` : "—"],
    [
      t("t274"),
      keyTest || t("t275"),
      result.key_test === "usable" ? "ok" : result.key_test ? "bad" : "",
    ],
    [t("t276"), routeLabel],
    [t("t094"), route.native_protocol || "—"],
    [
      t("t277"),
      t("t278", eligibility.allowed ? t("t302") : t("t303"), eligibility.source || "—"),
      eligibility.allowed ? "ok" : "warn",
    ],
  ]);
  $("#play-output").textContent = JSON.stringify(result.response, null, 2);
  renderDiagnostics();
}
$("#play-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  let request;
  try {
    request = JSON.parse($("#play-request").value);
  } catch (error) {
    toast(t("t279", error.message), true);
    return;
  }
  const keyValue = $("#play-key").value,
    key =
      keyValue === "auto"
        ? { mode: "auto" }
        : {
            mode: "selected",
            tier: keyValue.split(":")[0],
            id: keyValue.split(":").slice(1).join(":"),
          };
  $("#play-run").disabled = true;
  $("#play-state").textContent =
    key.mode === "selected" ? t("t280") : t("t281");
  try {
    const result = await api("/api/debug/inference", {
      method: "POST",
      body: JSON.stringify({ protocol: $("#play-protocol").value, key, request }),
    });
    displayPlayground(result);
  } catch (error) {
    toast(tx(error.message), true);
    $("#play-state").textContent = t("t282");
  } finally {
    $("#play-run").disabled = false;
  }
});
$("#play-copy").onclick = async () => {
  try {
    await navigator.clipboard.writeText($("#play-output").textContent);
    toast(t("t283"));
  } catch (_) {
    toast(t("t284"), true);
  }
};
function renderDiagnostics() {
  if (!monitorData && !debugData) return;
  const metadata = debugData?.metadata || monitorData?.resources?.metadata || {},
    last = lastPlayground || debugData?.last_inference;
  renderFacts("#metadata-facts", [
    [t("t285"), metadata.ready ? t("t286") : t("t287"), metadata.ready ? "ok" : "warn"],
    [t("t288"), formatTokens(metadata.models)],
    [t("t289"), metadata.updated_at ? new Date(metadata.updated_at).toLocaleString(LOC()) : t("t290")],
    [t("t291"), metadata.stale ? t("t286") : t("t287"), metadata.stale ? "warn" : "ok"],
    [t("t292"), metadata.next_refresh ? new Date(metadata.next_refresh).toLocaleString(LOC()) : "—"],
    [t("t293"), metadata.last_error || t("t294"), metadata.last_error ? "bad" : "ok"],
  ]);
  renderFacts(
    "#last-playground",
    last
      ? [
          [t("t173"), last.ok ? t("t295") : t("t296"), last.ok ? "ok" : "bad"],
          ["HTTP", last.http_status],
          [t("t104"), `${last.duration_ms} ms`],
          ["Request ID", last.request_id || "—"],
          [t("t025"), last.route?.model || "—"],
          [
            t("t095"),
            last.route?.tier
              ? `${last.route.tier} / ${last.route.anonymous ? "anonymous" : last.route.key_id || "key"}`
              : t("t268"),
          ],
        ]
      : [[t("t026"), t("t297")]],
  );
  renderDiagnosticModels();
  const requests = $("#upstream-requests");
  requests.replaceChildren();
  (monitorData?.upstream?.requests || [])
    .slice(-100)
    .reverse()
    .forEach((request) => {
      const row = document.createElement("tr");
      appendCell(row, new Date(request.time).toLocaleTimeString(LOC()));
      appendCell(row, request.request_id);
      appendCell(row, request.model);
      appendCell(row, request.tier || "—");
      appendCell(row, request.channel);
      appendCell(row, request.key_id || "—");
      appendCell(row, request.attempts);
      const terminalResults = {
        stream_error: t("t298"),
        client_canceled: t("t299"),
      };
      const result = terminalResults[request.outcome]
        ? `HTTP ${request.status} · ${terminalResults[request.outcome]}`
        : request.status || request.outcome;
      appendCell(row, result, request.success ? "ok" : "bad");
      appendCell(row, `${request.duration_ms} ms`);
      requests.appendChild(row);
    });
  if (!requests.children.length) emptyRow(requests, 9, t("t300"));
  const attempts = $("#upstream-attempts");
  attempts.replaceChildren();
  (monitorData?.upstream?.recent || [])
    .slice(-100)
    .reverse()
    .forEach((attempt) => {
      const row = document.createElement("tr");
      appendCell(row, new Date(attempt.time).toLocaleTimeString(LOC()));
      appendCell(row, attempt.request_id);
      appendCell(row, attempt.model);
      appendCell(row, `${attempt.tier} / ${attempt.channel}`);
      appendCell(row, attempt.key_id);
      appendCell(row, attempt.proxy_node);
      appendCell(row, attempt.status || attempt.outcome, attempt.success ? "ok" : "bad");
      appendCell(row, `${attempt.duration_ms} ms`);
      attempts.appendChild(row);
    });
  if (!attempts.children.length) emptyRow(attempts, 8, t("t301"));
}
function renderDiagnosticModels() {
  const host = $("#diagnostic-models");
  if (!host || !debugData) return;
  const query = $("#diagnostic-model-search").value.trim().toLowerCase();
  host.replaceChildren();
  (debugData.models || [])
    .filter((item) => !query || item.model.toLowerCase().includes(query))
    .forEach((item) => {
      const eligibility = item.anonymous_eligibility || {},
        row = document.createElement("tr"),
        input = eligibility.input_cost,
        output = eligibility.output_cost,
        plan = [...(item.anonymous ? ["anonymous"] : []), ...(item.key_tiers || [])];
      appendCell(row, item.model);
      appendCell(row, `${item.native_protocol} · ${item.protocol_source}`);
      appendCell(
        row,
        item.route_error || (plan.length ? plan.join(" → ") : "—"),
        item.route_error ? "bad" : "",
      );
      appendCell(row, eligibility.allowed ? t("t302") : t("t303"), eligibility.allowed ? "ok" : "warn");
      appendCell(row, eligibility.source || "—");
      appendCell(row, `${input == null ? "?" : input} / ${output == null ? "?" : output}`, "cost");
      host.appendChild(row);
    });
  if (!host.children.length) emptyRow(host, 6, t("t304"));
}
$("#diagnostic-model-search").addEventListener("input", renderDiagnosticModels);
async function refreshMonitor() {
  try {
    monitorData = await api("/api/monitor");
    const metrics = monitorData.metrics;
    $("#version").textContent = `version ${monitorData.version}`;
    $("#m-total").textContent = Number(metrics.last_hour.total || 0).toLocaleString(LOC());
    $("#m-rate").textContent = `${((metrics.last_hour.success_rate || 0) * 100).toFixed(1)}%`;
    $("#m-p95").textContent = `${metrics.last_hour.p95_ms || 0} ms`;
    $("#m-active").textContent = `${metrics.active_requests || 0} / ${metrics.active_streams || 0}`;
    renderResources(monitorData.resources);
    renderBreakdowns(metrics);
    renderGuide();
    renderUsage();
    renderDiagnostics();
  } catch (_) {}
}
$("#refresh-now").onclick = async () => {
  await Promise.all([refreshMonitor(), loadConfig(), loadDebugModels()]);
  if (location.hash === "#model-quotas") await loadModelQuotas();
  if (location.hash === "#api-keys") await loadAPIKeys();
  if (location.hash === "#proxies") await loadProxies();
  toast(t("t305"));
};
function derivedAPIBase() {
  if (!config) return `${location.protocol}//${location.hostname}:8080`;
  const listen = config.effective?.listen || config.listen || ":8080";
  const bracket = listen.match(/^\[.*\]:(\d+)$/),
    plain = listen.match(/:(\d+)$/),
    port = (bracket || plain)?.[1] || "8080";
  return `${location.protocol}//${location.hostname}:${port}`;
}
function codeCard(label, text) {
  const card = document.createElement("div"),
    head = document.createElement("div"),
    title = document.createElement("span"),
    copy = document.createElement("button"),
    pre = document.createElement("pre"),
    code = document.createElement("code");
  card.className = "code-card";
  head.className = "code-head";
  title.textContent = label;
  copy.type = "button";
  copy.className = "copy";
  copy.textContent = t("t087");
  copy.onclick = async () => {
    try {
      await navigator.clipboard.writeText(text);
      toast(t("t306"));
    } catch (_) {
      toast(t("t284"), true);
    }
  };
  head.append(title, copy);
  code.textContent = text;
  pre.appendChild(code);
  card.append(head, pre);
  return card;
}
function renderAccessExamples() {
  if (!config) return;
  const base = derivedAPIBase(),
    model = $("#access-model")?.value.trim() || "YOUR_MODEL";
  $("#access-base-value").value = base;
  const protocols = $("#access-protocol-examples"),
    sdks = $("#access-sdk-examples");
  protocols.replaceChildren();
  sdks.replaceChildren();
  const chat = `curl ${base}/v1/chat/completions \\\n  -H "Authorization: Bearer YOUR_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","messages":[{"role":"user","content":"Hello"}]}'`;
  const responses = `curl ${base}/v1/responses \\\n  -H "Authorization: Bearer YOUR_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","input":"Hello"}'`;
  const anthropic = `curl ${base}/v1/messages \\\n  -H "x-api-key: YOUR_API_KEY" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","max_tokens":512,"messages":[{"role":"user","content":"Hello"}]}'`;
  protocols.append(
    codeCard("Chat Completions", chat),
    codeCard("Responses", responses),
    codeCard("Anthropic Messages", anthropic),
  );
  const python = `from openai import OpenAI\n\nclient = OpenAI(base_url="${base}/v1", api_key="YOUR_API_KEY")\nresponse = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(response.choices[0].message.content)`;
  const javascript = `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  baseURL: "${base}/v1",\n  apiKey: "YOUR_API_KEY",\n});\nconst response = await client.responses.create({\n  model: "${model}",\n  input: "Hello",\n});\nconsole.log(response.output_text);`;
  sdks.append(
    codeCard("Python · OpenAI SDK", python),
    codeCard("JavaScript · OpenAI SDK", javascript),
  );
}
$("#access-model").addEventListener("input", renderAccessExamples);
$$("[data-scroll]").forEach(
  (button) =>
    (button.onclick = () =>
      document.getElementById(button.dataset.scroll).scrollIntoView({ behavior: "smooth" })),
);
function connectLogs() {
  if (eventSource) eventSource.close();
  eventSource = new EventSource("/api/logs/stream");
  eventSource.addEventListener("log", (event) => {
    const item = JSON.parse(event.data);
    logEvents.push(item);
    if (logEvents.length > 2000) logEvents.shift();
    if (!paused && !logRenderPending) {
      logRenderPending = true;
      requestAnimationFrame(() => {
        logRenderPending = false;
        renderLogs();
      });
    }
  });
  eventSource.addEventListener("gap", () => toast(t("t307"), true));
}
function renderLogs() {
  const level = $("#log-level").value,
    component = $("#log-component").value.toLowerCase(),
    query = $("#log-search").value.toLowerCase(),
    rows = logEvents
      .filter(
        (item) =>
          (!level || item.level === level) &&
          (!component || (item.component || "").toLowerCase().includes(component)) &&
          (!query || JSON.stringify(item).toLowerCase().includes(query)),
      )
      .slice(-500),
    host = $("#log-list");
  host.replaceChildren();
  rows.forEach((item) => {
    const row = document.createElement("div");
    row.className = "log";
    const timeNode = document.createElement("span"),
      levelNode = document.createElement("span"),
      componentNode = document.createElement("span"),
      messageNode = document.createElement("span"),
      fieldsNode = document.createElement("span");
    timeNode.className = "log-time";
    timeNode.textContent = new Date(item.time).toLocaleTimeString(LOC());
    levelNode.className =
      item.level === "error"
        ? "bad"
        : item.level === "warn"
          ? "warn"
          : item.level === "info"
            ? "ok"
            : "";
    levelNode.textContent = item.level;
    componentNode.className = "log-component";
    componentNode.textContent = item.component || "core";
    messageNode.textContent = item.message;
    fieldsNode.className = "log-fields";
    fieldsNode.textContent = JSON.stringify(item.fields || {});
    row.append(timeNode, levelNode, componentNode, messageNode, fieldsNode);
    host.appendChild(row);
  });
  if (!rows.length) {
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.textContent = t("t308");
    host.appendChild(empty);
  }
  host.scrollTop = host.scrollHeight;
}
["#log-level", "#log-component", "#log-search"].forEach((id) =>
  $(id).addEventListener("input", renderLogs),
);
$("#log-pause").onclick = () => {
  paused = !paused;
  $("#log-pause").textContent = paused ? t("t309") : t("t149");
  if (!paused) renderLogs();
};
let quotaData = null,
  quotaTimer = null,
  quotaLoading = false,
  createdKey = "",
  apiKeysLoading = false;
const localTime = (value) =>
  value ? new Date(value).toLocaleString(LOC(), { hour12: false }) : "—";

function clearCreatedKey() {
  createdKey = "";
  $("#api-key-env").textContent = "";
  $("#api-key-created").classList.add("hidden");
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast(t("t310"));
  } catch {
    toast(t("t311"), true);
  }
}

async function loadModelQuotas() {
  if (quotaLoading) return;
  quotaLoading = true;
  $("#quota-refresh").disabled = true;
  $("#quota-refresh").textContent = t("t153");
  try {
    const value = await api("/api/model-quotas");
    if (!csrf) return;
    quotaData = value;
    const select = $("#quota-account"),
      selected = select.value;
    select.replaceChildren();
    for (const account of value.accounts || []) {
      const option = document.createElement("option");
      option.value = account.id;
      option.textContent = `Go Key ${account.display}`;
      select.appendChild(option);
    }
    if ([...select.options].some((option) => option.value === selected)) select.value = selected;
    if (!select.options.length) select.appendChild(new Option(t("t312"), ""));
    renderModelQuotas();
  } catch (error) {
    $("#quota-status").textContent = t("t313", error.message);
  } finally {
    quotaLoading = false;
    $("#quota-refresh").disabled = false;
    $("#quota-refresh").textContent = t("t152");
  }
}

function renderModelQuotas() {
  if (!quotaData) return;
  const account = quotaData.accounts.find((item) => item.id === $("#quota-account").value);
  const stale = account?.status === "stale";
  const cards = $("#quota-cards");
  cards.replaceChildren();
  $("#quota-status").textContent = !account
    ? t("t314")
    : account.error
      ? t("t315", tx(account.error), t(stale ? "t432" : "t433"))
      : t("t316");
  $("#quota-updated").textContent = account?.fetched_at
    ? t("t317", t(stale ? "t424" : "t425"), localTime(account.fetched_at))
    : "";
  const windows = [
    ["rolling", t("t318")],
    ["weekly", t("t319")],
    ["monthly", t("t320")],
  ];
  for (const [id, label] of windows) {
    const item = account?.windows?.[id];
    const card = document.createElement("div"),
      heading = document.createElement("h3"),
      value = document.createElement("div"),
      progress = document.createElement("progress"),
      reset = document.createElement("div");
    card.className = `quota-card${item?.remaining_percent < 20 ? " quota-low" : ""}`;
    heading.textContent = label;
    value.className = "quota-value";
    value.textContent = item ? `≈ ${item.remaining_percent}%` : t("t321");
    progress.max = 100;
    if (item) progress.value = item.remaining_percent;
    else progress.value = 0;
    progress.setAttribute("aria-label", t("t322", label));
    reset.className = "quota-reset";
    reset.textContent = item
      ? t("t323", t(stale ? "t426" : "t427"), item.status === "rate-limited" ? t("t428") : "", localTime(item.resets_at))
      : t("t324");
    card.append(heading, value, progress, reset);
    cards.appendChild(card);
  }
  const query = $("#quota-model-search").value.trim().toLowerCase();
  const models = quotaData.models.filter((model) => model.id.toLowerCase().includes(query));
  $("#quota-model-count").textContent = `${models.length} / ${quotaData.models.length}`;
  const catalog = quotaData.catalog;
  $("#quota-catalog-status").textContent =
    t("t325", catalog?.stale ? t("t429") : "", localTime(catalog?.updated_at));
  const host = $("#quota-models");
  host.replaceChildren();
  for (const model of models) {
    const row = document.createElement("tr");
    appendCell(row, model.id, "mono");
    appendCell(
      row,
      { chat: "Chat Completions", responses: "Responses", anthropic: "Messages" }[model.protocol] ||
        model.protocol,
    );
    for (const [id] of windows) {
      const item = account?.windows?.[id];
      appendCell(row, item ? t("t326", item.remaining_percent, stale ? t("t430") : "") : t("t321"));
    }
    const cell = document.createElement("td"),
      button = document.createElement("button");
    button.className = "ghost";
    button.textContent = t("t327");
    button.onclick = () => copyText(model.id);
    cell.appendChild(button);
    row.appendChild(cell);
    host.appendChild(row);
  }
  if (!models.length)
    emptyRow(host, 6, quotaData.models.length ? t("t328") : t("t329"));
}
$("#quota-refresh").onclick = loadModelQuotas;
$("#quota-account").onchange = renderModelQuotas;
$("#quota-model-search").oninput = renderModelQuotas;

async function loadAPIKeys() {
  if (apiKeysLoading) return;
  apiKeysLoading = true;
  $("#api-key-refresh").disabled = true;
  try {
    const data = await api("/api/api-keys");
    if (csrf) renderAPIKeys(data.keys);
  } catch (error) {
    $("#api-key-status").textContent = t("t330", error.message);
  } finally {
    apiKeysLoading = false;
    $("#api-key-refresh").disabled = false;
  }
}

async function mutateAPIKey(id, method, body, button) {
  button.disabled = true;
  try {
    const data = await api(`/api/api-keys/${encodeURIComponent(id)}`, {
      method,
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!csrf) return;
    renderAPIKeys(data.keys);
    await loadConfig();
    toast(t("t331"));
  } catch (error) {
    toast(tx(error.message), true);
  } finally {
    button.disabled = false;
  }
}

function renderAPIKeys(keys) {
  $("#api-key-count").textContent = t("t332", keys.length);
  $("#api-key-status").textContent = t("t333", keys.filter((key) => key.enabled).length);
  const host = $("#api-key-list");
  host.replaceChildren();
  for (const key of keys) {
    const row = document.createElement("tr");
    appendCell(row, key.name);
    appendCell(row, key.display, "mono");
    appendCell(row, key.enabled ? t("t232") : t("t334"), key.enabled ? "status ok" : "status warn");
    const usage = key.usage || {},
      summary = (value = {}) => t("t335", formatTokens(value.requests), formatTokens(value.total_tokens));
    appendCell(row, `${summary(usage.last_hour)} / ${summary(usage.lifetime)}`);
    appendCell(row, key.created_at ? localTime(key.created_at) : t("t336"));
    const cell = document.createElement("td"),
      actions = document.createElement("div");
    actions.className = "key-actions";
    for (const [label, action] of [
      [
        t("t337"),
        (button) => {
          const name = prompt(t("t338"), key.name);
          if (name !== null) mutateAPIKey(key.id, "PATCH", { name }, button);
        },
      ],
      [
        key.enabled ? t("t339") : t("t340"),
        (button) => mutateAPIKey(key.id, "PATCH", { enabled: !key.enabled }, button),
      ],
      [
        t("t341"),
        (button) => {
          if (confirm(t("t342", key.name)))
            mutateAPIKey(key.id, "DELETE", null, button);
        },
      ],
    ]) {
      const button = document.createElement("button");
      button.className = "ghost";
      button.textContent = label;
      button.onclick = () => action(button);
      actions.appendChild(button);
    }
    cell.appendChild(actions);
    row.appendChild(cell);
    host.appendChild(row);
  }
  if (!keys.length) emptyRow(host, 6, t("t343"));
}
$("#api-key-form").onsubmit = async (event) => {
  event.preventDefault();
  const button = $("#api-key-create");
  button.disabled = true;
  try {
    const data = await api("/api/api-keys", {
      method: "POST",
      body: JSON.stringify({ name: $("#api-key-name").value, value: $("#api-key-value").value }),
    });
    if (!csrf) return;
    createdKey = data.key;
    const goModels = debugData?.models?.filter((item) => item.available_go) || [];
    const model =
      goModels.find((item) => item.model === "glm-5.3-flash")?.model ||
      goModels[0]?.model ||
      "YOUR_MODEL";
    $("#api-key-env").textContent =
      `OPENAI_API_KEY=${data.key}\nOPENAI_BASE_URL=${derivedAPIBase()}/v1\nOPENAI_MODEL=${model}`;
    $("#api-key-created").classList.remove("hidden");
    $("#api-key-form").reset();
    renderAPIKeys(data.keys);
    await loadConfig();
    toast(t("t344"));
  } catch (error) {
    toast(tx(error.message), true);
  } finally {
    button.disabled = false;
  }
};
$("#api-key-refresh").onclick = loadAPIKeys;
$("#api-key-dismiss").onclick = clearCreatedKey;
$("#api-key-copy").onclick = () => copyText(createdKey);
$("#api-key-copy-env").onclick = () => copyText($("#api-key-env").textContent);
let rotationData = null,
  rotationDirty = false;
function rotationReasons() {
  return {
    success: t("t295"),
    client_canceled: t("t345"),
    quota_or_rate_limit: t("t346"),
    transport_error: t("t347"),
    first_output_timeout: t("t348"),
    stream_error: t("t349"),
    response_read_error: t("t350"),
    response_conversion_error: t("t351"),
    request_conversion_error: t("t352"),
    upstream_error: t("t353"),
  };
}
async function loadRotation() {
  try {
    const value = await api("/api/rotation");
    if (!csrf) return;
    rotationData = value;
    rotationDirty = false;
    renderRotation();
  } catch (error) {
    toast(tx(error.message), true);
  }
}
function renderRotation() {
  const state = rotationData;
  $("#rotation-status").textContent =
    state.persistence_error || t("t354");
  $("#rotation-timeout").value = state.first_output_seconds;
  const host = $("#rotation-groups");
  host.replaceChildren();
  for (const group of state.groups) {
    const section = document.createElement("div");
    section.className = "section";
    const title = document.createElement("h2");
    title.textContent = group.id === "sota" ? "SOTA" : t("t355");
    const label = document.createElement("label");
    label.textContent = t("t356");
    label.htmlFor = `rotation-alias-${group.id}`;
    const input = document.createElement("input");
    input.id = label.htmlFor;
    input.value = group.alias;
    input.required = true;
    input.maxLength = 64;
    input.oninput = () => {
      group.alias = input.value;
      rotationDirty = true;
    };
    const field = document.createElement("div");
    field.className = "field";
    field.append(label, input);
    const current = document.createElement("p");
    current.className = "mono";
    current.textContent = t("t357", group.current || t("t431"));
    const table = document.createElement("table");
    table.className = "table";
    const header = document.createElement("tr");
    for (const text of [t("t358"), t("t025"), t("t359"), t("t032")]) {
      const th = document.createElement("th");
      th.textContent = text;
      header.append(th);
    }
    const thead = document.createElement("thead");
    thead.append(header);
    table.append(thead);
    const tbody = document.createElement("tbody");
    table.append(tbody);
    group.order.forEach((model, index) => {
      const row = document.createElement("tr");
      appendCell(row, String(index + 1));
      appendCell(row, model, "mono");
      const until = group.paused[model],
        paused = until && new Date(until) > new Date();
      appendCell(
        row,
        !group.available[model]
          ? t("t360")
          : paused
            ? t("t361", localTime(until))
            : model === group.current
              ? t("t362")
              : t("t234"),
      );
      const actions = document.createElement("td");
      actions.className = "rotation-actions";
      for (const [text, delta] of [
        [t("t363"), -1],
        [t("t364"), 1],
      ]) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "ghost";
        button.textContent = text;
        button.setAttribute("aria-label", `${model} ${text}`);
        button.disabled = index + delta < 0 || index + delta >= group.order.length;
        button.onclick = () => {
          [group.order[index], group.order[index + delta]] = [
            group.order[index + delta],
            group.order[index],
          ];
          state.first_output_seconds = Number($("#rotation-timeout").value);
          rotationDirty = true;
          renderRotation();
        };
        actions.append(button);
      }
      const select = document.createElement("button");
      select.type = "button";
      select.className = "ghost";
      select.textContent = t("t365");
      select.disabled = !group.available[model];
      select.setAttribute("aria-label", t("t366", model));
      select.onclick = async () => {
        select.disabled = true;
        try {
          const result = await api("/api/rotation/current", {
            method: "POST",
            body: JSON.stringify({ group: group.id, model }),
          });
          // Keep unsaved alias/order edits while refreshing operational state.
          for (const item of state.groups) {
            const live = result.groups.find((x) => x.id === item.id);
            Object.assign(item, {
              current: live.current,
              paused: live.paused,
              switches: live.switches,
            });
          }
          state.persistence_error = result.persistence_error;
          renderRotation();
          toast(t("t367"));
        } catch (error) {
          select.disabled = false;
          toast(tx(error.message), true);
        }
      };
      actions.append(select);
      row.append(actions);
      tbody.append(row);
    });
    if (!group.order.length) emptyRow(tbody, 4, t("t368"));
    const wrap = document.createElement("div");
    wrap.className = "table-wrap";
    wrap.append(table);
    const recent = document.createElement("div");
    recent.className = "rotation-switches";
    const caption = document.createElement("h3");
    caption.textContent = t("t369");
    recent.append(caption);
    for (const item of (group.switches || []).slice(-5).reverse()) {
      const line = document.createElement("p");
      line.textContent = `${localTime(item.time)} · ${item.from} → ${item.to} · ${(rotationReasons()[item.reason] || item.reason)}`;
      recent.append(line);
    }
    if (!group.switches?.length) {
      const line = document.createElement("p");
      line.textContent = t("t370");
      recent.append(line);
    }
    section.append(title, field, current, wrap, recent);
    host.append(section);
  }
  const attempts = $("#rotation-attempts");
  attempts.replaceChildren();
  for (const item of (state.attempts || []).slice().reverse()) {
    const row = document.createElement("tr");
    for (const value of [
      localTime(item.time),
      item.request,
      item.alias,
      item.model,
      String(item.number),
      (rotationReasons()[item.outcome] || item.outcome),
    ])
      appendCell(row, value);
    attempts.append(row);
  }
  if (!state.attempts?.length) emptyRow(attempts, 6, t("t371"));
}
$("#rotation-refresh").onclick = () => {
  if (rotationDirty) {
    toast(t("t372"), true);
    return;
  }
  loadRotation();
};
$("#rotation-timeout").oninput = () => {
  rotationDirty = true;
};
$("#rotation-form").onsubmit = async (event) => {
  event.preventDefault();
  if (!rotationData) return;
  const button = $("#rotation-save");
  button.disabled = true;
  try {
    const body = { first_output_seconds: Number($("#rotation-timeout").value) };
    for (const group of rotationData.groups)
      body[group.id] = { alias: group.alias.trim(), order: group.order };
    rotationData = await api("/api/rotation", { method: "PUT", body: JSON.stringify(body) });
    rotationDirty = false;
    renderRotation();
    toast(t("t373"));
  } catch (error) {
    toast(tx(error.message), true);
  } finally {
    button.disabled = false;
  }
};
let proxiesLoading = false,
  proxyMode = "roundrobin";
const proxyModeDesc = { sticky: "t439", roundrobin: "t440", random: "t441" };
async function loadProxies() {
  if (proxiesLoading) return;
  proxiesLoading = true;
  $("#proxy-refresh").disabled = true;
  try {
    const data = await api("/api/proxies");
    if (!csrf) return;
    proxyMode = data.proxy_mode || "roundrobin";
    $("#proxy-mode").value = proxyMode;
    $("#proxy-mode-desc").textContent = t(proxyModeDesc[proxyMode] || "t440");
    renderProxies(data.proxies || []);
  } catch (error) {
    $("#proxy-status").textContent = t("t472", error.message);
  } finally {
    proxiesLoading = false;
    $("#proxy-refresh").disabled = false;
  }
}
function renderProxies(proxies) {
  $("#proxy-count").textContent = t("t471", proxies.length, proxies.filter((p) => p.enabled).length);
  $("#proxy-status").textContent = "";
  const host = $("#proxy-list");
  host.replaceChildren();
  for (const proxy of proxies) {
    const row = document.createElement("tr");
    appendCell(row, proxy.source === "proxyfile" ? proxy.address + " [" + proxy.source + "]" : proxy.address, "mono");
    appendCell(row, proxy.healthy ? t("t453") : t("t454"), proxy.healthy ? "status ok" : "status bad");
    appendCell(row, proxy.enabled ? t("t455") : t("t456"), proxy.enabled ? "status ok" : "status warn");
    appendCell(row, t("t231", proxy.zen_keys, proxy.go_keys, proxy.anonymous ? t("t422") : t("t423")));
    const cell = document.createElement("td"),
      actions = document.createElement("div");
    actions.className = "key-actions";
    const proxyActs = [[t("t457"), (button) => testProxy(proxy, button)]];
    if (proxy.source !== "proxyfile") {
      proxyActs.push([t("t458"), (button) => editProxy(proxy, button)]);
      proxyActs.push([proxy.enabled ? t("t461") : t("t460"), (button) => mutateProxy(proxy.id, "PATCH", { enabled: !proxy.enabled }, button, "t468")]);
      proxyActs.push([t("t459"), (button) => {
        if (confirm(t("t465"))) mutateProxy(proxy.id, "DELETE", null, button, "t469");
      }]);
    }
    for (const [label, action] of proxyActs) {
      const button = document.createElement("button");
      button.className = "ghost";
      button.textContent = label;
      button.onclick = () => action(button);
      actions.appendChild(button);
    }
    cell.appendChild(actions);
    row.appendChild(cell);
    host.appendChild(row);
  }
  if (!host.children.length) emptyRow(host, 5);
}
async function mutateProxy(id, method, body, button, okMsg) {
  button.disabled = true;
  try {
    const data = await api(`/api/proxies/${encodeURIComponent(id)}`, {
      method,
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!csrf) return;
    proxyMode = data.proxy_mode || proxyMode;
    $("#proxy-mode").value = proxyMode;
    $("#proxy-mode-desc").textContent = t(proxyModeDesc[proxyMode] || "t440");
    renderProxies(data.proxies || []);
    await loadConfig();
    toast(t(okMsg));
  } catch (error) {
    toast(tx(error.message), true);
  } finally {
    button.disabled = false;
  }
}
async function testProxy(proxy, button) {
  button.disabled = true;
  const old = button.textContent;
  button.textContent = t("t462");
  try {
    const data = await api("/api/proxies/test", { method: "POST", body: JSON.stringify({ id: proxy.id }) });
    if (!csrf) return;
    toast(data.ok ? t("t463", data.address, data.http_status, data.latency_ms) : t("t464", data.address, data.detail), !data.ok);
    loadProxies();
  } catch (error) {
    toast(tx(error.message), true);
  } finally {
    button.disabled = false;
    button.textContent = old;
  }
}
function editProxy(proxy, button) {
  const value = prompt(t("t466"), "");
  if (value === null || !value.trim()) return;
  mutateProxy(proxy.id, "PATCH", { value: value.trim() }, button, "t468");
}
$("#proxy-refresh").onclick = () => loadProxies();
$("#proxy-mode").onchange = async (event) => {
  const mode = event.target.value;
  try {
    const data = await api("/api/proxies/mode", { method: "PUT", body: JSON.stringify({ mode }) });
    if (!csrf) return;
    proxyMode = data.proxy_mode || mode;
    $("#proxy-mode-desc").textContent = t(proxyModeDesc[proxyMode] || "t440");
    renderProxies(data.proxies || []);
    await loadConfig();
    toast(t("t470"));
  } catch (error) {
    toast(tx(error.message), true);
    $("#proxy-mode").value = proxyMode;
  }
};
$("#proxy-add-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const lines = $("#proxy-add-lines").value.split(/\r?\n/);
  try {
    const data = await api("/api/proxies", { method: "POST", body: JSON.stringify({ lines }) });
    if (!csrf) return;
    $("#proxy-add-lines").value = "";
    renderProxies(data.proxies || []);
    await loadConfig();
    toast(t("t467"));
  } catch (error) {
    toast(tx(error.message), true);
  }
});
boot();
// ---- i18n: re-render dynamic content on language change ----
window.o2aLangChanged = () => {
  const page = (location.hash || "#overview").slice(1);
  if (pages[page]) openPage(page);
  else openPage("overview");
  // openPage covers monitor/guide/usage/quotas/rotation/availability/api-keys;
  // force-debug pages too in case they hold stale strings.
  try {
    if (typeof renderDiagnostics === "function") renderDiagnostics();
  } catch (_) {}
  try {
    if (typeof resetPlayTemplate === "function" && !$("#play-request").value.trim()) resetPlayTemplate();
  } catch (_) {}
};
for (const sel of ["#lang-switch", "#login-lang"]) {
  const node = $(sel);
  if (node) {
    node.value = window.o2aI18n.currentLang();
    node.addEventListener("change", () => window.o2aI18n.setLang(node.value));
  }
}


async function loadAvailability() {
  try {
    const data = await api("/api/models/availability");
    $("#availability-error").textContent = "";
    $("#availability-models").replaceChildren();
    for (const item of data.models) {
      const row = document.createElement("tr");
      const date = (value) =>
        value && !value.startsWith("0001-") ? new Date(value).toLocaleString(LOC()) : t("t374");
      for (const value of [
        item.model,
        item.disabled ? t("t375") : t("t232"),
        [item.channel, item.reason].filter(Boolean).join(" / ") || t("t374"),
        item.manual ? t("t376") : t("t377"),
        item.auto_effort?.effort
          ? t("t378", item.auto_effort.effort, date(item.effort_checked_at))
          : t("t379"),
        date(item.checked_at),
        item.manual ? t("t380") : date(item.next_check),
      ]) {
        const cell = document.createElement("td");
        cell.textContent = value;
        row.append(cell);
      }
      const cell = document.createElement("td");
      const button = document.createElement("button");
      button.textContent = t("t381");
      button.disabled = !item.disabled;
      button.onclick = async () => {
        button.disabled = true;
        toggle.disabled = true;
        try {
          await api("/api/models/restore", {
            method: "POST",
            body: JSON.stringify({ model: item.model }),
          });
          await loadAvailability();
          await loadDebugModels();
        } catch (error) {
          $("#availability-error").textContent = error.message;
          button.disabled = !item.disabled;
          toggle.disabled = false;
        }
      };
      cell.append(button);
      const toggle = document.createElement("button");
      toggle.className = "ghost";
      toggle.textContent = item.manual ? t("t382") : t("t383");
      toggle.onclick = async () => {
        button.disabled = true;
        toggle.disabled = true;
        try {
          await api("/api/models/manual", {
            method: "POST",
            body: JSON.stringify({ model: item.model, manual: !item.manual }),
          });
          await loadAvailability();
          await loadDebugModels();
        } catch (error) {
          $("#availability-error").textContent = error.message;
          button.disabled = !item.disabled;
          toggle.disabled = false;
        }
      };
      cell.append(toggle);
      row.append(cell);
      $("#availability-models").append(row);
    }
    if (!data.models.length)
      $("#availability-error").textContent = t("t384");
  } catch (error) {
    $("#availability-error").textContent = error.message;
  }
}
$("#availability-refresh").onclick = loadAvailability;
