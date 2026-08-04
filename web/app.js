"use strict";

const state = {
  token: localStorage.getItem("relay_token") || "",
  role: localStorage.getItem("relay_role") || "",
  userID: localStorage.getItem("relay_user_id") || "",
  username: "",
  view: "",
};

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

function fmt(n) {
  return new Intl.NumberFormat("zh-CN").format(Number(n || 0));
}

function pct(used, limit) {
  const p = limit > 0 ? Math.min(100, Math.round((used / limit) * 100)) : 0;
  return p;
}

function usageCell(used, limit) {
  const p = pct(used, limit);
  return `<span class="usage-bar"><span style="width:${p}%"></span></span><span class="usage-pct">${p}%</span>`;
}

function toast(msg, isError) {
  const el = $("#toast");
  el.textContent = msg;
  el.classList.toggle("error", !!isError);
  el.classList.remove("hidden");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => el.classList.add("hidden"), 3200);
}

async function api(path, options = {}) {
  const opts = { ...options, headers: { ...(options.headers || {}) } };
  if (state.token) opts.headers.Authorization = "Bearer " + state.token;
  if (opts.body && typeof opts.body !== "string") {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401 && !path.endsWith("/login")) {
    logout();
    throw new Error("登录已过期");
  }
  let data = null;
  const text = await res.text();
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    const msg = data && data.error && data.error.message ? data.error.message : `请求失败 (${res.status})`;
    throw new Error(msg);
  }
  return data;
}

function logout() {
  state.token = "";
  state.role = "";
  state.userID = "";
  localStorage.removeItem("relay_token");
  localStorage.removeItem("relay_role");
  localStorage.removeItem("relay_user_id");
  $("#main-view").classList.add("hidden");
  $("#login-view").classList.remove("hidden");
}

function showView(name) {
  state.view = name;
  $$(".view").forEach((v) => v.classList.add("hidden"));
  $$(".nav-btn").forEach((b) => b.classList.toggle("active", b.dataset.view === name));
  $("#view-" + name).classList.remove("hidden");
  const loaders = {
    users: loadUsers,
    pool: loadPool,
    reports: loadReports,
    overview: loadOverview,
    mykeys: loadMyKeys,
    myreports: loadMyReports,
  };
  if (loaders[name]) loaders[name]();
}

function modal(html) {
  $("#modal-root").innerHTML = `<div class="modal-backdrop"><div class="modal">${html}</div></div>`;
  $("#modal-root").querySelector(".modal-backdrop").addEventListener("mousedown", (e) => {
    if (e.target.classList.contains("modal-backdrop")) closeModal();
  });
}

function closeModal() {
  $("#modal-root").innerHTML = "";
}

function modalHead(title) {
  return `<div class="modal-head"><h2>${esc(title)}</h2><button class="btn btn-ghost" data-close type="button">关闭</button></div>`;
}

function modalFoot(actions) {
  return `<div class="modal-foot">${actions}</div>`;
}

// ---------- login ----------

$("#login-view").classList.toggle("hidden", !!state.token);
$("#main-view").classList.toggle("hidden", !state.token);

let loginRole = "admin";
$$(".seg-btn").forEach((b) => b.addEventListener("click", () => {
  loginRole = b.dataset.role;
  $$(".seg-btn").forEach((x) => x.classList.toggle("active", x === b));
}));

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#login-error").textContent = "";
  const username = $("#login-username").value.trim();
  const password = $("#login-password").value;
  try {
    const data = await api(`/${loginRole}/login`, {
      method: "POST",
      body: { username, password },
    });
    state.token = data.token;
    state.role = loginRole;
    localStorage.setItem("relay_token", state.token);
    localStorage.setItem("relay_role", state.role);
    $("#login-view").classList.add("hidden");
    $("#main-view").classList.remove("hidden");
    enterApp();
  } catch (err) {
    $("#login-error").textContent = err.message;
  }
});

function enterApp() {
  const isAdmin = state.role === "admin";
  $("#nav-admin").hidden = !isAdmin;
  $("#nav-user").hidden = isAdmin;
  if (isAdmin) {
    state.userID = "";
    localStorage.removeItem("relay_user_id");
    $("#whoami").textContent = "管理员";
    showView("users");
  } else {
    api("/user/me").then((me) => {
      state.userID = me.id;
      state.username = me.username;
      localStorage.setItem("relay_user_id", me.id);
      $("#whoami").textContent = me.username || me.id;
      showView("overview");
    }).catch(() => showView("overview"));
  }
}

$("#logout-btn").addEventListener("click", logout);
$$(".nav-btn").forEach((b) => b.addEventListener("click", () => showView(b.dataset.view)));

// ---------- users (admin) ----------

$("#btn-new-user").addEventListener("click", () => {
  modal(`
    ${modalHead("新建用户")}
    <div class="modal-body">
      <label>用户名<input id="u-username" required></label>
      <label>显示名<input id="u-display"></label>
      <label>初始密码<input id="u-password" type="password" required></label>
      <label class="muted">小时额度（0 = 默认 1000 万）<input id="u-hourly" type="number" min="0" value="0"></label>
      <label class="muted">日额度（0 = 默认 4 亿）<input id="u-daily" type="number" min="0" value="0"></label>
      <label class="muted">启用<input id="u-enabled" type="checkbox" checked></label>
    </div>
    ${modalFoot('<button class="btn" data-close type="button">取消</button><button class="btn btn-primary" id="u-submit" type="button">创建</button>')}
  `);
  bindClose();
  $("#u-submit").addEventListener("click", async () => {
    try {
      const data = await api("/admin/users", {
        method: "POST",
        body: {
          username: $("#u-username").value.trim(),
          display_name: $("#u-display").value.trim(),
          password: $("#u-password").value,
          enabled: $("#u-enabled").checked,
          hourly_tokens: Number($("#u-hourly").value || 0),
          daily_tokens: Number($("#u-daily").value || 0),
        },
      });
      closeModal();
      showAccessKey(data.access_key, "中转 Key 已签发，请立即保存");
      loadUsers();
    } catch (err) { toast(err.message, true); }
  });
});

function bindClose() {
  $$("[data-close]").forEach((b) => b.addEventListener("click", closeModal));
}

function showAccessKey(key, title) {
  modal(`
    ${modalHead(title)}
    <div class="modal-body">
      <label>中转 Key
        <div class="key-reveal"><span class="mono">${esc(key)}</span></div>
      </label>
    </div>
    ${modalFoot('<button class="btn btn-primary" data-close type="button">已保存</button>')}
  `);
  bindClose();
}

async function downloadCSV(path, filename) {
  try {
    const text = await api(path);
    const blob = new Blob([String(text)], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  } catch (err) { toast(err.message, true); }
}

async function loadUsers() {
  try {
    const data = await api("/admin/users");
    const rows = data.users.map((u) => `
      <tr>
        <td>${esc(u.username)}<br><span class="mono muted">${esc(u.id)}</span></td>
        <td>${esc(u.display_name || "-")}</td>
        <td>${u.enabled ? '<span class="badge badge-on">启用</span>' : '<span class="badge badge-off">停用</span>'}</td>
        <td>${fmt(u.hourly_used)} / ${fmt(u.hourly_limit)}<br>${usageCell(u.hourly_used, u.hourly_limit)}</td>
        <td>${fmt(u.daily_used)} / ${fmt(u.daily_limit)}<br>${usageCell(u.daily_used, u.daily_limit)}</td>
        <td>${u.access_keys} / ${u.upstream_keys}</td>
        <td><div class="row-actions">
          <button class="btn btn-sm" data-act="quota" data-id="${esc(u.id)}" type="button">额度</button>
          <button class="btn btn-sm" data-act="password" data-id="${esc(u.id)}" type="button">密码</button>
          <button class="btn btn-sm" data-act="access" data-id="${esc(u.id)}" type="button">中转 Key</button>
          <button class="btn btn-sm" data-act="keys" data-id="${esc(u.id)}" type="button">上游 Key</button>
          <button class="btn btn-sm" data-act="toggle" data-id="${esc(u.id)}" data-enabled="${u.enabled}" type="button">${u.enabled ? "停用" : "启用"}</button>
          <button class="btn btn-sm btn-danger" data-act="delete" data-id="${esc(u.id)}" type="button">删除</button>
        </div></td>
      </tr>`).join("");
    $("#users-table").innerHTML = `<thead><tr>
      <th>用户</th><th>显示名</th><th>状态</th><th>小时用量</th><th>日用量</th><th>Key 数</th><th>操作</th>
    </tr></thead><tbody>${rows || '<tr><td colspan="7" class="empty">暂无用户</td></tr>'}</tbody>`;
    $$("#users-table [data-act]").forEach((btn) => btn.addEventListener("click", userAction));
  } catch (err) { toast(err.message, true); }
}

function userAction(e) {
  const btn = e.currentTarget;
  const id = btn.dataset.id;
  const act = btn.dataset.act;
  if (act === "quota") quotaModal(id);
  if (act === "password") passwordModal(id);
  if (act === "access") accessKeysModal(id);
  if (act === "keys") keysModal(id, "admin");
  if (act === "toggle") toggleUser(id, btn.dataset.enabled === "true");
  if (act === "delete") deleteUser(id);
}

function quotaModal(id) {
  api(`/admin/usage/user/${id}`).then((u) => {
    modal(`
      ${modalHead("配置额度")}
      <div class="modal-body">
        <label>小时额度（0 = 默认 ${fmt(10000000)}）<input id="q-hourly" type="number" min="0" value="${u.hourly_limit}"></label>
        <label>日额度（0 = 默认 ${fmt(400000000)}）<input id="q-daily" type="number" min="0" value="${u.daily_limit}"></label>
      </div>
      ${modalFoot('<button class="btn" data-close type="button">取消</button><button class="btn btn-primary" id="q-submit" type="button">保存</button>')}
    `);
    bindClose();
    $("#q-submit").addEventListener("click", async () => {
      try {
        await api(`/admin/users/${id}/quota`, {
          method: "PUT",
          body: { hourly_tokens: Number($("#q-hourly").value || 0), daily_tokens: Number($("#q-daily").value || 0) },
        });
        closeModal();
        toast("额度已更新");
        loadUsers();
      } catch (err) { toast(err.message, true); }
    });
  }).catch((err) => toast(err.message, true));
}

function passwordModal(id) {
  modal(`
    ${modalHead("重置密码")}
    <div class="modal-body">
      <label>新密码<input id="p-password" type="password"></label>
    </div>
    ${modalFoot('<button class="btn" data-close type="button">取消</button><button class="btn btn-primary" id="p-submit" type="button">重置</button>')}
  `);
  bindClose();
  $("#p-submit").addEventListener("click", async () => {
    try {
      await api(`/admin/users/${id}/password`, { method: "POST", body: { password: $("#p-password").value } });
      closeModal();
      toast("密码已重置");
    } catch (err) { toast(err.message, true); }
  });
}

async function accessKeysModal(id) {
  try {
    const data = await api(`/admin/keys/${id}`);
    const user = data.keys;
    modal(`
      ${modalHead("上游 Key")}
      <div class="modal-body">
        ${keysTable(user, "admin", id)}
        <hr>
        <label>名称<input id="k-name"></label>
        <label>Base URL<input id="k-base" placeholder="https://api.deepseek.com"></label>
        <label>API Key<input id="k-apikey" type="password"></label>
        <label>模型（逗号分隔，留空 = 全部）<input id="k-models"></label>
        <label>小时上限（0 = 默认）<input id="k-hourly" type="number" min="0" value="0"></label>
        <label class="muted">启用<input id="k-enabled" type="checkbox" checked></label>
      </div>
      ${modalFoot('<button class="btn" data-close type="button">关闭</button><button class="btn btn-primary" id="k-submit" type="button">添加</button>')}
    `);
    bindClose();
    $("#k-submit").addEventListener("click", () => addKey(id, "admin"));
  } catch (err) { toast(err.message, true); }
}

function keysTable(keys, scope, owner) {
  if (!keys.length) return '<div class="empty">暂无上游 Key</div>';
  return `<div class="table-wrap"><table class="table"><thead><tr><th>名称</th><th>Base URL</th><th>模型</th><th>状态</th><th>用量</th><th>在途</th><th>操作</th></tr></thead><tbody>` +
    keys.map((k) => `<tr>
      <td>${esc(k.name || "-")}<br><span class="mono muted">${esc(k.id)}</span></td>
      <td class="mono">${esc(k.base_url)}</td>
      <td>${k.models.length ? esc(k.models.join(", ")) : "全部"}</td>
      <td>${k.enabled ? '<span class="badge badge-on">启用</span>' : '<span class="badge badge-off">停用</span>'}</td>
      <td>${fmt(k.hourly_used)}${usageCell(k.hourly_used, k.hourly_limit)}</td>
      <td>${k.in_flight}</td>
      <td><div class="row-actions">
        <button class="btn btn-sm" data-owner="${esc(owner || "")}" data-kid="${esc(k.id)}" data-act="edit" type="button">编辑</button>
        <button class="btn btn-sm btn-danger" data-owner="${esc(owner || "")}" data-kid="${esc(k.id)}" data-act="delete" type="button">删除</button>
      </div></td>
    </tr>`).join("") + `</tbody></table></div>`;
}

async function addKey(userID, scope) {
  try {
    const path = scope === "admin" ? `/admin/keys/${userID}` : "/user/keys";
    await api(path, {
      method: "POST",
      body: {
        name: $("#k-name").value.trim(),
        base_url: $("#k-base").value.trim(),
        api_key: $("#k-apikey").value,
        models: splitModels($("#k-models").value),
        enabled: $("#k-enabled").checked,
        hourly_limit: Number($("#k-hourly").value || 0),
      },
    });
    closeModal();
    toast("Key 已添加");
    if (scope === "admin") keysModal(userID, "admin"); else loadMyKeys();
  } catch (err) { toast(err.message, true); }
}

function splitModels(v) {
  return v.split(/[,，]/).map((s) => s.trim()).filter(Boolean);
}

function keysModal(userID, scope) {
  const path = scope === "admin" ? `/admin/keys/${userID}` : "/user/keys";
  api(path).then((data) => {
    modal(`
      ${modalHead(scope === "admin" ? "上游 Key 管理" : "我的上游 Key")}
      <div class="modal-body">
        ${keysTable(data.keys, scope, scope === "admin" ? userID : state.userID)}
        <label>名称<input id="k-name"></label>
        <label>Base URL<input id="k-base" placeholder="https://api.deepseek.com"></label>
        <label>API Key<input id="k-apikey" type="password"></label>
        <label>模型（逗号分隔，留空 = 全部）<input id="k-models"></label>
        <label>小时上限（0 = 默认）<input id="k-hourly" type="number" min="0" value="0"></label>
        <label class="muted">启用<input id="k-enabled" type="checkbox" checked></label>
      </div>
      ${modalFoot('<button class="btn" data-close type="button">关闭</button><button class="btn btn-primary" id="k-submit" type="button">添加</button>')}
    `);
    bindClose();
    $("#k-submit").addEventListener("click", () => addKey(userID, scope));
    bindKeyRows(scope);
  }).catch((err) => toast(err.message, true));
}

function bindKeyRows(scope) {
  $$("[data-kid]").forEach((btn) => btn.addEventListener("click", async (e) => {
    const kid = e.currentTarget.dataset.kid;
    const owner = e.currentTarget.dataset.owner;
    if (e.currentTarget.dataset.act === "delete") {
      const path = scope === "admin"
        ? `/admin/keys/${owner}/${kid}`
        : `/user/keys/${kid}`;
      if (!confirm("删除该上游 Key？")) return;
      try {
        await api(path, { method: "DELETE" });
        toast("Key 已删除");
        if (scope === "admin") keysModal(owner, "admin"); else loadMyKeys();
      } catch (err) { toast(err.message, true); }
      return;
    }
    // edit uses generic edit modal with key id + owner path
    editKeyModal(kid, scope, owner);
  }));
}

function editKeyModal(kid, scope, owner) {
  const listPath = scope === "admin" ? `/admin/keys/${owner}` : "/user/keys";
  api(listPath).then((data) => {
    const k = data.keys.find((x) => x.id === kid);
    if (!k) return;
    modal(`
      ${modalHead("编辑上游 Key")}
      <div class="modal-body">
        <label>名称<input id="e-name" value="${esc(k.name || "")}"></label>
        <label>Base URL<input id="e-base" value="${esc(k.base_url)}"></label>
        <label>API Key（留空保持不变）<input id="e-apikey" type="password" placeholder="不修改请留空"></label>
        <label>模型（逗号分隔，留空 = 全部）<input id="e-models" value="${esc(k.models.join(", "))}"></label>
        <label>小时上限（0 = 默认）<input id="e-hourly" type="number" min="0" value="${k.hourly_limit}"></label>
        <label class="muted">启用<input id="e-enabled" type="checkbox" ${k.enabled ? "checked" : ""}></label>
      </div>
      ${modalFoot('<button class="btn" data-close type="button">取消</button><button class="btn btn-primary" id="e-submit" type="button">保存</button>')}
    `);
    bindClose();
    $("#e-submit").addEventListener("click", async () => {
      const body = {
        name: $("#e-name").value.trim(),
        base_url: $("#e-base").value.trim(),
        models: splitModels($("#e-models").value),
        enabled: $("#e-enabled").checked,
        hourly_limit: Number($("#e-hourly").value || 0),
      };
      const pw = $("#e-apikey").value;
      if (pw) body.api_key = pw;
      try {
        const path = scope === "admin"
          ? `/admin/keys/${owner}/${kid}`
          : `/user/keys/${kid}`;
        await api(path, { method: "PUT", body });
        closeModal();
        toast("Key 已更新");
        if (scope === "admin") keysModal(owner, "admin"); else loadMyKeys();
      } catch (err) { toast(err.message, true); }
    });
  }).catch((err) => toast(err.message, true));
}

async function toggleUser(id, enabled) {
  try {
    await api(`/admin/users/${id}/enabled`, { method: "PUT", body: { enabled: !enabled } });
    loadUsers();
  } catch (err) { toast(err.message, true); }
}

async function deleteUser(id) {
  if (!confirm("删除用户及其全部 Key？")) return;
  try {
    await api(`/admin/users/${id}`, { method: "DELETE" });
    toast("用户已删除");
    loadUsers();
  } catch (err) { toast(err.message, true); }
}

// ---------- pool (admin) ----------

$("#btn-refresh-pool").addEventListener("click", loadPool);

async function loadPool() {
  try {
    const data = await api("/admin/keys");
    const rows = data.keys.map((k) => `
      <tr>
        <td>${esc(k.name || "-")}<br><span class="mono muted">${esc(k.id)}</span></td>
        <td><span class="mono">${esc(k.user_id)}</span></td>
        <td class="mono">${esc(k.base_url)}</td>
        <td>${k.models.length ? esc(k.models.join(", ")) : "全部"}</td>
        <td>${k.enabled ? '<span class="badge badge-on">启用</span>' : '<span class="badge badge-off">停用</span>'}</td>
        <td>${fmt(k.hourly_used)}${usageCell(k.hourly_used, k.hourly_limit || 10000000)}</td>
        <td>${k.in_flight}</td>
        <td>${k.has_key ? '<span class="badge badge-own">已配置</span>' : '<span class="badge badge-off">缺失</span>'}</td>
      </tr>`).join("");
    $("#pool-table").innerHTML = `<thead><tr>
      <th>名称</th><th>属主</th><th>Base URL</th><th>模型</th><th>状态</th><th>小时用量</th><th>在途</th><th>密钥</th>
    </tr></thead><tbody>${rows || '<tr><td colspan="8" class="empty">暂无上游 Key</td></tr>'}</tbody>`;
  } catch (err) { toast(err.message, true); }
}

// ---------- reports (admin) ----------

$("#btn-load-report").addEventListener("click", loadReports);
$("#btn-export-csv").addEventListener("click", () => {
  downloadCSV(`/admin/reports/monthly?month=${$("#report-month").value}&format=csv`, `report-${$("#report-month").value}.csv`);
});

async function loadReports() {
  const month = $("#report-month").value;
  try {
    const data = await api(`/admin/reports/monthly?month=${month}`);
    renderReport(data, "#report-summary", "#report-table");
  } catch (err) { toast(err.message, true); }
}

function renderReport(data, summarySel, tableSel) {
  const s = data.summary;
  $(summarySel).innerHTML = `
    <div class="stat-card"><div class="label">请求数</div><div class="value">${fmt(s.requests)}</div></div>
    <div class="stat-card"><div class="label">Prompt Tokens</div><div class="value">${fmt(s.prompt_tokens)}</div></div>
    <div class="stat-card"><div class="label">Completion Tokens</div><div class="value">${fmt(s.completion_tokens)}</div></div>
    <div class="stat-card"><div class="label">总 Tokens</div><div class="value">${fmt(s.total_tokens)}</div></div>
    <div class="stat-card"><div class="label">错误</div><div class="value">${fmt(s.errors)}</div></div>
    <div class="stat-card"><div class="label">平均延迟</div><div class="value">${fmt(s.avg_latency_ms)} ms</div></div>
    <div class="stat-card"><div class="label">最大延迟</div><div class="value">${fmt(s.max_latency_ms)} ms</div></div>`;
  const rows = data.rows.map((r) => `
    <tr>
      <td><span class="mono">${esc(r.user_id)}</span></td>
      <td>${esc(r.model)}</td>
      <td>${esc(r.day)}</td>
      <td>${fmt(r.requests)}</td>
      <td>${fmt(r.prompt_tokens)}</td>
      <td>${fmt(r.completion_tokens)}</td>
      <td>${fmt(r.total_tokens)}</td>
      <td>${fmt(r.errors)}</td>
      <td>${fmt(r.avg_latency_ms)}</td>
      <td>${fmt(r.max_latency_ms)}</td>
    </tr>`).join("");
  $(tableSel).innerHTML = `<thead><tr>
    <th>用户</th><th>模型</th><th>日期</th><th>请求</th><th>Prompt</th><th>Completion</th><th>总 Tokens</th><th>错误</th><th>平均延迟</th><th>最大延迟</th>
  </tr></thead><tbody>${rows || '<tr><td colspan="10" class="empty">本月暂无数据</td></tr>'}</tbody>`;
}

// ---------- user self service ----------

async function loadOverview() {
  try {
    const me = await api("/user/me");
    const keys = await api("/user/keys");
    const usage = await api("/user/usage");
    $("#overview-content").innerHTML = `
      <div class="stat-grid">
        <div class="stat-card"><div class="label">用户名</div><div class="value">${esc(me.username)}</div></div>
        <div class="stat-card"><div class="label">小时用量</div><div class="value">${fmt(usage.hourly_used)} / ${fmt(usage.hourly_limit)}</div></div>
        <div class="stat-card"><div class="label">日用量</div><div class="value">${fmt(usage.daily_used)} / ${fmt(usage.daily_limit)}</div></div>
        <div class="stat-card"><div class="label">工作时段</div><div class="value">${usage.working_hour ? "生效中" : "未生效"}</div></div>
        <div class="stat-card"><div class="label">每分钟限制</div><div class="value">${usage.per_minute_limit}</div></div>
        <div class="stat-card"><div class="label">上游 Key</div><div class="value">${keys.keys.length}</div></div>
      </div>
      <div class="table-wrap"><table class="table"><thead><tr><th>中转 Key</th></tr></thead><tbody>
        ${me.access_keys.map((k) => `<tr><td class="mono">${esc(k.key)}</td></tr>`).join("") || '<tr><td class="empty">暂无中转 Key，请联系管理员</td></tr>'}
      </tbody></table></div>`;
  } catch (err) { toast(err.message, true); }
}

$("#btn-new-mykey").addEventListener("click", () => keysModal(state.userID, "user"));

async function loadMyKeys() {
  try {
    const data = await api("/user/keys");
    $("#mykeys-table").innerHTML = keysTable(data.keys, "user", state.userID);
    bindKeyRows("user");
  } catch (err) { toast(err.message, true); }
}

$("#btn-load-myreport").addEventListener("click", loadMyReports);

async function loadMyReports() {
  const month = $("#myreport-month").value;
  try {
    const data = await api(`/user/reports?month=${month}`);
    renderReport(data, "#myreport-summary", "#myreport-table");
  } catch (err) { toast(err.message, true); }
}

// boot
if (state.token) enterApp();
