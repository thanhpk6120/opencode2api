// Page: 代理管理 — list, bulk-add, mode selection (sticky, roundrobin, random), test reachability and latency.
import { api, hasSession } from "../api.js";
import { loadConfig } from "../data.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, confirmDialog, promptDialog, toast } from "../ui.js";

let proxiesLoading = false;
let currentProxies = [];
let proxyMode = "roundrobin";
let proxySource = "both";
let rotatingSettings = {};

const modeDescriptions = () => ({
  sticky: t(
    "固定使用单个代理，请求出错（5xx 或网络中断）时自动故障切换至下一个可用代理。",
    "proxies.mode.descSticky",
  ),
  roundrobin: t(
    "按列表顺序轮询分发每个请求至可用代理。",
    "proxies.mode.descRoundrobin",
  ),
  random: t(
    "每个请求随机选取一个可用代理。",
    "proxies.mode.descRandom",
  ),
});

async function load() {
  if (proxiesLoading) return;
  proxiesLoading = true;
  const btnRefresh = $("#proxy-refresh");
  if (btnRefresh) btnRefresh.disabled = true;
  try {
    const data = await api("/api/proxies");
    if (hasSession()) {
      proxyMode = data.proxy_mode || "roundrobin";
      proxySource = data.proxy_source || "both";
      rotatingSettings = data.rotating_proxy || {};
      currentProxies = data.proxies || [];
      render();
    }
  } catch (error) {
    const status = $("#proxy-status");
    if (status) {
      status.textContent = t(
        `读取失败：${error.message}`,
        "proxies.loadFailed",
        error.message,
      );
    }
  } finally {
    proxiesLoading = false;
    if (btnRefresh) btnRefresh.disabled = false;
  }
}

async function setMode(newMode) {
  const select = $("#proxy-mode");
  if (select) select.disabled = true;
  try {
    const data = await api("/api/proxies/mode", {
      method: "POST",
      body: JSON.stringify({ mode: newMode }),
    });
    if (!hasSession()) return;
    proxyMode = data.proxy_mode || newMode;
    updateModeDisplay();
    await loadConfig();
    toast(t("代理模式已更新", "proxies.toast.modeChanged"));
  } catch (error) {
    toast(error.message, true);
    if (select) select.value = proxyMode;
  } finally {
    if (select) select.disabled = false;
  }
}

function updateSettingsDisplay() {
  const source = $("#proxy-source");
  if (source) source.value = proxySource;
  const auto = $("#proxy-auto-rotate");
  if (auto) auto.checked = Boolean(rotatingSettings.auto_rotate);
  const min = $("#proxy-rotate-min");
  if (min) min.value = rotatingSettings.min_interval_seconds ?? 60;
  const max = $("#proxy-rotate-max");
  if (max) max.value = rotatingSettings.max_interval_seconds ?? 1800;
  const cache = $("#proxy-rotate-cache");
  if (cache) cache.value = rotatingSettings.failed_ip_cache_size ?? 10;
}

async function saveSettings(button) {
  if (button) button.disabled = true;
  try {
    const data = await api("/api/proxies/settings", {
      method: "PUT",
      body: JSON.stringify({
        proxy_source: $("#proxy-source")?.value || "both",
        rotating_proxy: {
          auto_rotate: Boolean($("#proxy-auto-rotate")?.checked),
          min_interval_seconds: Number($("#proxy-rotate-min")?.value || 0),
          max_interval_seconds: Number($("#proxy-rotate-max")?.value || 0),
          failed_ip_cache_size: Number($("#proxy-rotate-cache")?.value || 10),
        },
      }),
    });
    if (!hasSession()) return;
    proxySource = data.proxy_source || "both";
    rotatingSettings = data.rotating_proxy || {};
    currentProxies = data.proxies || [];
    render();
    await loadConfig();
    toast(t("代理设置已保存", "proxies.source.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

function updateModeDisplay() {
  const select = $("#proxy-mode");
  const desc = $("#proxy-mode-desc");
  if (select) select.value = proxyMode;
  if (desc) {
    const descs = modeDescriptions();
    desc.textContent = descs[proxyMode] || descs.roundrobin;
  }
}

async function testProxy(proxy, button) {
  if (button) button.disabled = true;
  const originalText = button?.textContent;
  if (button) button.textContent = t("检测中…", "proxies.testing");
  try {
    const res = await api("/api/proxies/test", {
      method: "POST",
      body: JSON.stringify({ id: proxy.id }),
    });
    if (res.ok) {
      toast(
        t(
          `测试成功：${res.address} (HTTP ${res.http_status}, ${res.latency_ms}ms)`,
          "proxies.test.ok",
          res.address,
          res.http_status,
          res.latency_ms,
        ),
      );
    } else {
      toast(
        t(
          `测试失败：${res.address} (${res.detail || "不可达"})`,
          "proxies.test.failed",
          res.address,
          res.detail || "unreachable",
        ),
        true,
      );
    }
    await load();
  } catch (error) {
    toast(error.message, true);
  } finally {
    if (button) {
      button.textContent = originalText;
      button.disabled = false;
    }
  }
}

async function testAllProxies(button) {
  if (button) button.disabled = true;
  const enabled = currentProxies.filter((p) => p.enabled);
  if (!enabled.length) {
    toast(t("当前没有已启用的代理可供测试", "proxies.noEnabledToTest"), true);
    if (button) button.disabled = false;
    return;
  }
  toast(t(`开始测试 ${enabled.length} 个代理…`, "proxies.testingAll", enabled.length));
  for (const proxy of enabled) {
    try {
      await api("/api/proxies/test", {
        method: "POST",
        body: JSON.stringify({ id: proxy.id }),
      });
    } catch (_) {}
  }
  await load();
  toast(t("全部代理测试完成", "proxies.testingAllDone"));
  if (button) button.disabled = false;
}

async function rotateProxy(proxy, button) {
  if (button) button.disabled = true;
  const original = button?.textContent;
  if (button) button.textContent = t("轮换中…", "proxies.rotating");
  try {
    const data = await api(`/api/proxies/${encodeURIComponent(proxy.id)}/rotate`, {
      method: "POST",
    });
    if (!hasSession()) return;
    currentProxies = data.proxies || [];
    render();
    toast(t("已调用轮换链接，出口 IP 已更换", "proxies.toast.rotated"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    if (button) {
      button.textContent = original;
      button.disabled = false;
    }
  }
}

async function mutateProxy(id, method, body, button) {
  if (button) button.disabled = true;
  try {
    const data = await api(`/api/proxies/${encodeURIComponent(id)}`, {
      method,
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!hasSession()) return;
    currentProxies = data.proxies || [];
    render();
    await loadConfig();
    toast(t("操作成功并已生效", "proxies.toast.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

function render() {
  updateModeDisplay();
  updateSettingsDisplay();
  const countEl = $("#proxy-count");
  const statusEl = $("#proxy-status");
  if (countEl) {
    countEl.textContent = t(
      `${currentProxies.length} 个，${currentProxies.filter((p) => p.enabled).length} 个已启用`,
      "proxies.count",
      currentProxies.length,
      currentProxies.filter((p) => p.enabled).length,
    );
  }
  if (statusEl) statusEl.textContent = "";

  const host = $("#proxy-list");
  if (!host) return;
  host.replaceChildren();

  for (const proxy of currentProxies) {
    const row = el("tr");

    // Address
    const addrText =
      proxy.source === "proxyfile"
        ? `${proxy.address} [${proxy.source}]`
        : proxy.address;
    row.append(el("td", { class: "mono", text: addrText }));

    // Rotate link (exit-IP change endpoint)
    row.append(
      el("td", {
        class: "section-caption mono",
        text: proxy.rotate_link
          ? proxy.rotate_link
          : t("未配置", "proxies.rotate.none"),
      }),
    );

    // Health
    let healthClass = "status bad";
    let healthText = t("故障", "proxies.health.bad");
    if (proxy.checking) {
      healthClass = "status warn";
      healthText = t("检测中…", "proxies.health.checking");
    } else if (proxy.healthy) {
      healthClass = "status ok";
      healthText = t("健康", "proxies.health.ok");
    }
    row.append(el("td", {}, [el("span", { class: healthClass, text: healthText })]));

    // State (Enabled / Disabled)
    const stateClass = proxy.enabled ? "status ok" : "status warn";
    const stateText = proxy.enabled
      ? t("已启用", "proxies.state.enabled")
      : t("已禁用", "proxies.state.disabled");
    row.append(el("td", {}, [el("span", { class: stateClass, text: stateText })]));

    // Binding (Zen, Go, Anonymous)
    const anonText = proxy.anonymous
      ? t("是", "common.yes")
      : t("否", "common.no");
    const bindingText = t(
      `Zen: ${proxy.zen_keys} | Go: ${proxy.go_keys} | 匿名: ${anonText}`,
      "proxies.binding.desc",
      proxy.zen_keys,
      proxy.go_keys,
      anonText,
    );
    row.append(el("td", { class: "section-caption", text: bindingText }));

    // Actions
    const actions = el("div", { style: "display:flex;gap:6px;flex-wrap:wrap" });
    const btnTest = el("button", {
      class: "btn btn-sm btn-ghost",
      text: t("测试", "proxies.action.test"),
      click: () => testProxy(proxy, btnTest),
    });
    actions.append(btnTest);

    if (proxy.source !== "proxyfile") {
      const btnEdit = el("button", {
        class: "btn btn-sm btn-ghost",
        text: t("编辑", "proxies.action.edit"),
        click: async () => {
          const val = await promptDialog({
            title: t("编辑代理地址", "proxies.edit.title"),
            label: t("代理地址（host:port:user:pass 或完整 URL）", "proxies.edit.label"),
            value: proxy.address,
            confirmLabel: t("保存", "common.save"),
          });
          if (val && val.trim() && val.trim() !== proxy.address) {
            mutateProxy(proxy.id, "PATCH", { address: val.trim() }, btnEdit);
          }
        },
      });
      actions.append(btnEdit);

      const btnLink = el("button", {
        class: "btn btn-sm btn-ghost",
        text: t("配置轮换链接", "proxies.action.rotateLink"),
        click: async () => {
          const val = await promptDialog({
            title: t("配置 IP 轮换链接", "proxies.rotate.linkTitle"),
            label: t("调用后会更换出口 IP 的链接（留空清除）", "proxies.rotate.linkLabel"),
            value: proxy.rotate_link || "",
            placeholder: "https://provider.example.com/changeip?token=...",
            confirmLabel: t("保存", "common.save"),
            maxlength: 512,
          });
          if (val === null) return;
          mutateProxy(proxy.id, "PATCH", { rotate_link: val.trim() }, btnLink);
        },
      });
      actions.append(btnLink);

      const btnRotate = el("button", {
        class: "btn btn-sm btn-ghost",
        text: t("轮换 IP", "proxies.action.rotate"),
        disabled: !proxy.rotate_link,
        click: () => rotateProxy(proxy, btnRotate),
      });
      actions.append(btnRotate);

      const btnToggle = el("button", {
        class: "btn btn-sm btn-ghost",
        text: proxy.enabled
          ? t("禁用", "proxies.action.disable")
          : t("启用", "proxies.action.enable"),
        click: () =>
          mutateProxy(
            proxy.id,
            "PATCH",
            { enabled: !proxy.enabled },
            btnToggle,
          ),
      });
      actions.append(btnToggle);

      const btnDelete = el("button", {
        class: "btn btn-sm btn-danger",
        text: t("删除", "proxies.action.delete"),
        click: async () => {
          const confirmed = await confirmDialog({
            title: t("确认删除", "proxies.delete.title"),
            message: t(`确认删除代理 ${proxy.address} 吗？`, "proxies.delete.confirm", proxy.address),
            danger: true,
            confirmLabel: t("删除", "common.delete"),
          });
          if (confirmed) {
            mutateProxy(proxy.id, "DELETE", null, btnDelete);
          }
        },
      });
      actions.append(btnDelete);
    }

    row.append(el("td", {}, [actions]));
    host.append(row);
  }

  if (!currentProxies.length) {
    emptyRow(host, 6, t("暂无配置代理", "proxies.empty"));
  }
}

export default {
  id: "proxies",
  title: "代理管理",
  sub: "配置出口代理、故障切换策略与多协议支持（HTTP / HTTPS / SOCKS5）。",
  init() {
    $("#proxy-refresh")?.addEventListener("click", load);
    $("#proxy-test-all")?.addEventListener("click", (e) => testAllProxies(e.currentTarget));
    $("#proxy-mode")?.addEventListener("change", (e) => setMode(e.target.value));
    $("#proxy-settings-save")?.addEventListener("click", (e) => saveSettings(e.currentTarget));

    $("#proxy-add-form")?.addEventListener("submit", async (e) => {
      e.preventDefault();
      const input = $("#proxy-add-lines");
      if (!input) return;
      const lines = input.value.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
      if (!lines.length) return;
      const btnAdd = $("#proxy-add");
      if (btnAdd) btnAdd.disabled = true;
      try {
        const rotateLink = ($("#proxy-add-rotate")?.value || "").trim();
        const data = await api("/api/proxies", {
          method: "POST",
          body: JSON.stringify({ lines, rotate_link: rotateLink }),
        });
        if (!hasSession()) return;
        input.value = "";
        currentProxies = data.proxies || [];
        render();
        await loadConfig();
        toast(t("代理添加成功", "proxies.toast.added"));
        const rotateInput = $("#proxy-add-rotate");
        if (rotateInput) rotateInput.value = "";
      } catch (error) {
        toast(error.message, true);
      } finally {
        if (btnAdd) btnAdd.disabled = false;
      }
    });

    onChange(() => {
      if (currentProxies) render();
    });
  },
  show: load,
};
