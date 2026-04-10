package operator

import (
	"bytes"
	"html/template"
)

type bootstrapPayload struct {
	Overview  Overview            `json:"overview"`
	Services  []ServiceDescriptor `json:"services"`
	Examples  []QuickStartExample `json:"examples"`
	PortalURL string              `json:"portal_url"`
}

func (s *Service) Bootstrap() bootstrapPayload {
	overview := s.store.Overview()
	return bootstrapPayload{
		Overview:  overview,
		Services:  SupportedServices(),
		Examples:  QuickStartExamples(overview.Endpoint),
		PortalURL: overview.Endpoint + "/_stratus/",
	}
}

func (s *Service) PortalPage() ([]byte, error) {
	tmpl, err := template.New("portal").Funcs(template.FuncMap{
		"json": templateJSON,
	}).Parse(operatorPortalHTML)
	if err != nil {
		return nil, err
	}
	data := s.Bootstrap()
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

const operatorPortalHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Stratus</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f6f9fc;
      --panel: rgba(255,255,255,.78);
      --panel-strong: rgba(255,255,255,.9);
      --line: rgba(15,23,42,.1);
      --line-strong: rgba(15,23,42,.18);
      --text: #0f172a;
      --muted: #526174;
      --soft: #7b8798;
      --brand: #0f172a;
      --blue: #7ea9ff;
      --blue-soft: rgba(126,169,255,.18);
      --ok: #0f766e;
      --warn: #b45309;
      --err: #b91c1c;
      --shadow: 0 30px 80px rgba(15,23,42,.08);
      --radius: 28px;
    }
    * { box-sizing: border-box; }
    html, body { margin: 0; padding: 0; }
    body {
      min-height: 100vh;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: var(--text);
      background:
        radial-gradient(circle at top, rgba(138,181,255,.24), transparent 30rem),
        radial-gradient(circle at 78% 12%, rgba(255,255,255,.95), transparent 18rem),
        linear-gradient(180deg, #f8fbff 0%, #edf3f8 45%, #f8fafc 100%);
    }
    .wrap { max-width: 1280px; margin: 0 auto; padding: 16px 20px 56px; }
    .shell {
      border: 1px solid rgba(255,255,255,.76);
      background: rgba(255,255,255,.68);
      backdrop-filter: blur(24px);
      border-radius: 28px;
      box-shadow: var(--shadow);
      padding: 16px 18px;
      display: flex;
      gap: 16px;
      align-items: center;
      justify-content: space-between;
      position: sticky;
      top: 16px;
      z-index: 30;
    }
    .brand { display: flex; align-items: center; gap: 14px; }
    .badge {
      width: 44px; height: 44px; border-radius: 18px;
      display: grid; place-items: center;
      background: var(--brand); color: white;
      box-shadow: 0 14px 32px rgba(15,23,42,.22);
      font-size: 20px;
    }
    .eyebrow {
      margin: 0 0 4px; font-size: 11px; letter-spacing: .32em;
      text-transform: uppercase; font-weight: 700; color: var(--soft);
    }
    .brand h1 { margin: 0; font-size: 18px; letter-spacing: -.03em; }
    .top-note {
      border: 1px solid var(--line);
      background: rgba(255,255,255,.7);
      border-radius: 999px;
      padding: 8px 12px;
      font-size: 12px;
      color: var(--muted);
    }
    .hero {
      margin-top: 18px;
      position: relative;
      overflow: hidden;
      border-radius: 28px;
      border: 1px solid rgba(255,255,255,.72);
      background: linear-gradient(180deg, rgba(255,255,255,.82) 0%, rgba(255,255,255,.72) 100%);
      backdrop-filter: blur(24px);
      box-shadow: var(--shadow);
      padding: 30px 26px;
      display: grid;
      grid-template-columns: minmax(0, 1.35fr) minmax(320px, .9fr);
      gap: 22px;
    }
    .hero::after {
      content: "";
      position: absolute;
      inset: 0 0 0 auto;
      width: 24rem;
      background: radial-gradient(circle at center, rgba(126,169,255,.14), transparent 64%);
      pointer-events: none;
    }
    .hero-copy { position: relative; z-index: 1; }
    .hero h2 {
      margin: 0;
      max-width: 760px;
      font-size: clamp(38px, 5vw, 62px);
      letter-spacing: -.06em;
      line-height: .95;
      font-weight: 800;
    }
    .hero p {
      margin: 16px 0 0;
      max-width: 680px;
      color: var(--muted);
      font-size: 16px;
      line-height: 1.75;
    }
    .hero-actions {
      display: flex;
      flex-wrap: wrap;
      gap: 12px;
      margin-top: 22px;
    }
    .hero-side {
      position: relative;
      z-index: 1;
      display: grid;
      gap: 14px;
    }
    .hero-side-card {
      border-radius: 24px;
      border: 1px solid var(--line);
      background: rgba(248, 250, 252, .84);
      padding: 18px;
    }
    .hero-side-card strong {
      display: block;
      margin-top: 10px;
      font-size: 19px;
      letter-spacing: -.04em;
    }
    .mini-list {
      display: grid;
      gap: 10px;
      margin: 14px 0 0;
      padding: 0;
      list-style: none;
    }
    .mini-list li {
      display: flex;
      align-items: start;
      gap: 10px;
      color: var(--muted);
      font-size: 13px;
      line-height: 1.6;
    }
    .mini-dot {
      width: 8px;
      height: 8px;
      border-radius: 999px;
      margin-top: 6px;
      background: var(--blue);
    }
    .pill {
      display: inline-flex;
      align-items: center;
      gap: 10px;
      padding: 12px 16px;
      border-radius: 999px;
      border: 1px solid var(--line);
      background: rgba(255,255,255,.78);
      color: var(--text);
      text-decoration: none;
      font-size: 13px;
      font-weight: 600;
    }
    .pill.primary {
      background: var(--brand);
      color: white;
      border-color: var(--brand);
      box-shadow: 0 16px 34px rgba(15,23,42,.2);
    }
    .grid { display: grid; gap: 18px; margin-top: 18px; }
    .stats { grid-template-columns: repeat(4, minmax(0, 1fr)); }
    .two-up { grid-template-columns: 1.45fr 1fr; }
    .three-up { grid-template-columns: 1.1fr 1.1fr 1.4fr; }
    .panel {
      border: 1px solid rgba(255,255,255,.72);
      background: rgba(255,255,255,.74);
      backdrop-filter: blur(24px);
      border-radius: var(--radius);
      box-shadow: 0 12px 32px rgba(15,23,42,.05);
      padding: 22px;
    }
    .panel h3 { margin: 0; font-size: 22px; letter-spacing: -.04em; }
    .panel-head {
      display: flex; align-items: center; justify-content: space-between; gap: 12px;
      margin-bottom: 16px;
    }
    .micro {
      font-size: 11px;
      font-weight: 700;
      letter-spacing: .28em;
      text-transform: uppercase;
      color: var(--soft);
      margin: 0 0 8px;
    }
    .stat-value {
      font-size: clamp(22px, 2.6vw, 32px);
      letter-spacing: -.05em;
      font-weight: 700;
      line-height: 1.02;
      word-break: break-word;
    }
    .stat-value.endpoint,
    .stat-value.path {
      font-size: clamp(18px, 2.2vw, 26px);
      line-height: 1.04;
    }
    .section-tag {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      padding: 7px 10px;
      border-radius: 999px;
      border: 1px solid var(--line);
      background: rgba(255,255,255,.74);
      color: var(--muted);
      font-size: 12px;
      font-weight: 600;
    }
    .hint { margin-top: 10px; font-size: 14px; line-height: 1.6; color: var(--muted); }
    .service-grid, .examples { display: grid; gap: 14px; }
    .service-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); align-items: start; }
    .service-card, .example-card {
      border-radius: 24px;
      border: 1px solid var(--line);
      background: rgba(255,255,255,.72);
      padding: 16px;
      min-width: 0;
    }
    .service-top {
      display: flex;
      flex-direction: column;
      align-items: start;
      gap: 10px;
      margin-bottom: 10px;
    }
    .service-name {
      margin: 0;
      font-size: 16px;
      font-weight: 700;
      letter-spacing: -.02em;
      line-height: 1.1;
      word-break: break-word;
    }
    .mode-tag, .tone-tag {
      display: inline-flex; align-items: center; justify-content: center;
      padding: 4px 10px; border-radius: 999px;
      border: 1px solid var(--line);
      font-size: 11px; font-weight: 700; letter-spacing: .12em; text-transform: uppercase;
      color: var(--muted);
      background: rgba(255,255,255,.8);
      white-space: nowrap;
      max-width: 100%;
    }
    .service-meta {
      font-size: 12px; letter-spacing: .16em; text-transform: uppercase; color: var(--soft);
      margin-bottom: 10px;
    }
    .service-summary, .example-card p {
      margin: 0;
      color: var(--muted);
      font-size: 14px;
      line-height: 1.6;
    }
    pre {
      margin: 14px 0 0;
      overflow: auto;
      border-radius: 20px;
      border: 1px solid rgba(15,23,42,.08);
      background: #0f172a;
      color: #e5eefb;
      padding: 16px;
      font-size: 12px;
      line-height: 1.65;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    }
    .toolbar { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; }
    .toolbar button { font: inherit; }
    select {
      appearance: none;
      border: 1px solid var(--line);
      border-radius: 999px;
      padding: 10px 14px;
      background: rgba(255,255,255,.84);
      color: var(--text);
      font: inherit;
      min-width: 160px;
    }
    table { width: 100%; border-collapse: collapse; }
    th, td { text-align: left; padding: 12px 10px; border-top: 1px solid var(--line); vertical-align: top; }
    th {
      font-size: 10px; letter-spacing: .26em; text-transform: uppercase; color: var(--soft);
      font-weight: 700;
      border-top: none;
    }
    td { font-size: 14px; color: var(--muted); }
    td strong { color: var(--text); font-weight: 700; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; }
    .status {
      display: inline-flex; align-items: center; gap: 8px;
      padding: 6px 10px; border-radius: 999px; font-size: 12px; font-weight: 700;
      border: 1px solid transparent;
    }
    .ok { background: rgba(16,185,129,.12); color: var(--ok); border-color: rgba(16,185,129,.16); }
    .warn { background: rgba(245,158,11,.14); color: var(--warn); border-color: rgba(245,158,11,.18); }
    .err { background: rgba(239,68,68,.12); color: var(--err); border-color: rgba(239,68,68,.18); }
    .empty { padding: 34px 0; color: var(--soft); text-align: center; }
    .log-shell {
      border-radius: 26px;
      border: 1px solid rgba(15,23,42,.1);
      background: linear-gradient(180deg, #0f172a 0%, #111827 100%);
      color: #e5eefb;
      overflow: hidden;
      box-shadow: 0 30px 80px rgba(15,23,42,.22);
    }
    .log-head { padding: 18px 20px; border-bottom: 1px solid rgba(255,255,255,.08); }
    .log-body { max-height: 32rem; overflow: auto; padding: 18px 20px; }
    .log-line {
      display: grid; grid-template-columns: 160px 1fr; gap: 14px;
      padding: 12px 14px;
      border: 1px solid rgba(255,255,255,.06);
      border-radius: 18px;
      background: rgba(255,255,255,.03);
      margin-bottom: 10px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 12px;
      line-height: 1.6;
    }
    .muted { color: var(--soft); }
    .copy {
      cursor: pointer;
      background: transparent;
    }
    @media (max-width: 1024px) {
      .hero { grid-template-columns: 1fr; }
      .stats, .two-up, .three-up { grid-template-columns: 1fr 1fr; }
      .service-grid { grid-template-columns: 1fr; }
    }
    @media (max-width: 720px) {
      .wrap { padding: 12px 14px 40px; }
      .shell { border-radius: 24px; }
      .hero { padding: 26px 20px 22px; border-radius: 24px; }
      .stats, .two-up, .three-up { grid-template-columns: 1fr; }
      .log-line { grid-template-columns: 1fr; }
      .top-note { display: none; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <header class="shell">
      <div class="brand">
        <div class="badge">☁</div>
        <div>
          <p class="eyebrow">Stratus</p>
          <h1>Local Operator Portal</h1>
        </div>
      </div>
    </header>

    <section class="hero">
      <div class="hero-copy">
        <p class="eyebrow">Local AWS Loop</p>
        <h2>Connect real AWS tooling to a local emulator without losing operational visibility.</h2>
        <p>Use this portal to get your endpoint, inspect the supported service surface, copy working examples, and watch recent activity, failures, and CloudWatch-style logs from the same local Stratus process.</p>
        <div class="hero-actions">
          <a class="pill primary" href="#examples">Quick start examples</a>
          <a class="pill" href="#services">Available services</a>
          <a class="pill" href="#logs">Live logs</a>
        </div>
      </div>
      <aside class="hero-side">
        <div class="hero-side-card">
          <p class="micro">Running Now</p>
          <strong>{{.Overview.Endpoint}}</strong>
          <ul class="mini-list">
            <li><span class="mini-dot"></span><span>Use <code class="mono">stratus serve</code> for scripts and CI.</span></li>
            <li><span class="mini-dot"></span><span>Use <code class="mono">stratus</code> or <code class="mono">stratus dev</code> for the portal-first flow.</span></li>
            <li><span class="mini-dot"></span><span>The built-in portal lives at <code class="mono">/_stratus/</code> on the same server.</span></li>
          </ul>
        </div>
        <div class="hero-side-card">
          <p class="micro">What You Get</p>
          <strong>Examples, services, activity, failures, logs.</strong>
        </div>
      </aside>
    </section>

    <section class="grid stats" id="stats">
      <div class="panel">
        <p class="micro">Endpoint</p>
        <div class="stat-value endpoint" id="stat-endpoint">{{.Overview.Endpoint}}</div>
        <div class="hint">AWS CLI, SDKs, CDK, and Preflight should point here.</div>
      </div>
      <div class="panel">
        <p class="micro">Requests</p>
        <div class="stat-value" id="stat-requests">{{.Overview.TotalRequests}}</div>
        <div class="hint">Recent in-memory operator window from the running server.</div>
      </div>
      <div class="panel">
        <p class="micro">Uptime</p>
        <div class="stat-value" id="stat-uptime">{{.Overview.UptimeSeconds}}s</div>
        <div class="hint" id="stat-log-mode">Log {{.Overview.LogLevel}} / {{.Overview.LogFormat}}</div>
      </div>
      <div class="panel">
        <p class="micro">Data Dir</p>
        <div class="stat-value path" id="stat-data-dir">{{.Overview.DataDir}}</div>
        <div class="hint">Durable metadata and payload storage for local runs.</div>
      </div>
    </section>

    <section class="grid two-up">
      <div class="panel" id="services">
        <div class="panel-head">
          <div>
            <p class="micro">Supported Services</p>
            <h3>Available local AWS surface</h3>
          </div>
          <div class="tone-tag" id="service-count">{{len .Services}} services</div>
        </div>
        <div class="service-grid" id="service-grid"></div>
      </div>
      <div class="panel" id="examples">
        <div class="panel-head">
          <div>
            <p class="micro">Quick Start</p>
            <h3>Connect to Stratus</h3>
          </div>
        </div>
        <div class="examples" id="examples-grid"></div>
      </div>
    </section>

    <section class="grid two-up">
      <div class="panel">
        <div class="panel-head">
          <div>
            <p class="micro">Recent Activity</p>
            <h3>Latest request stream</h3>
          </div>
          <div class="toolbar">
            <button class="pill copy" type="button" id="refresh-activity">Refresh</button>
          </div>
        </div>
        <div style="overflow:auto">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>Service</th>
                <th>Operation</th>
                <th>Status</th>
                <th>Duration</th>
              </tr>
            </thead>
            <tbody id="activity-body"></tbody>
          </table>
        </div>
      </div>
      <div class="panel">
        <div class="panel-head">
          <div>
            <p class="micro">Recent Failures</p>
            <h3>Last errors returned</h3>
          </div>
        </div>
        <div style="overflow:auto">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>Service</th>
                <th>Status</th>
                <th>Error</th>
              </tr>
            </thead>
            <tbody id="errors-body"></tbody>
          </table>
        </div>
      </div>
    </section>

    <section class="grid three-up" id="logs">
      <div class="panel">
        <div class="panel-head">
          <div>
            <p class="micro">Log Groups</p>
            <h3>CloudWatch-style browsing</h3>
          </div>
          <div class="toolbar">
            <button class="pill copy" type="button" id="refresh-logs">Refresh</button>
          </div>
        </div>
        <div class="toolbar">
          <select id="group-select"></select>
        </div>
        <div class="hint">Choose a log group emitted by Lambda or other local workflows.</div>
      </div>
      <div class="panel">
        <div class="panel-head">
          <div>
            <p class="micro">Streams</p>
            <h3>Execution streams</h3>
          </div>
        </div>
        <div class="toolbar">
          <select id="stream-select"></select>
        </div>
        <div class="hint">Switch between streams to inspect specific function runs or emitted sequences.</div>
      </div>
      <div class="log-shell">
        <div class="log-head">
          <p class="micro" style="color:#94a3b8;margin-bottom:8px">Live Logs</p>
          <strong id="log-title">Select a log group and stream</strong>
        </div>
        <div class="log-body" id="log-body"></div>
      </div>
    </section>
  </div>
  <script>
    const services = {{json .Services}};
    const examples = {{json .Examples}};
    const $ = (id) => document.getElementById(id);

    function escapeHtml(value) {
      return String(value ?? "")
        .replaceAll("&", "&amp;")
        .replaceAll("<", "&lt;")
        .replaceAll(">", "&gt;")
        .replaceAll('"', "&quot;");
    }

    function formatTime(value) {
      return new Intl.DateTimeFormat("en-CA", {
        month: "short",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
      }).format(new Date(value));
    }

    function statusClass(status) {
      if (status >= 500) return "err";
      if (status >= 400) return "warn";
      return "ok";
    }

    function formatDuration(ms) {
      if (ms < 1000) return ms + "ms";
      return (ms / 1000).toFixed(2) + "s";
    }

    async function fetchJSON(path) {
      const response = await fetch(path, { cache: "no-store" });
      if (!response.ok) {
        const body = await response.text();
        throw new Error(path + ": " + response.status + " " + body);
      }
      return response.json();
    }

    function renderServices() {
      $("service-grid").innerHTML = services.map((service) =>
        '<article class="service-card">' +
          '<div class="service-top">' +
            '<h4 class="service-name">' + escapeHtml(service.name) + '</h4>' +
            '<span class="mode-tag">' + escapeHtml(service.mode) + '</span>' +
          '</div>' +
          '<div class="service-meta">' + escapeHtml(service.category) + '</div>' +
          '<p class="service-summary">' + escapeHtml(service.summary) + '</p>' +
        '</article>'
      ).join("");
    }

    function renderExamples() {
      $("examples-grid").innerHTML = examples.map((example, idx) =>
        '<article class="example-card">' +
          '<div class="panel-head" style="margin-bottom:0">' +
            '<div>' +
              '<p class="micro">' + escapeHtml(example.title) + '</p>' +
              '<h3 style="font-size:18px">' + escapeHtml(example.description) + '</h3>' +
            '</div>' +
            '<button class="pill copy" type="button" data-copy="' + idx + '">Copy</button>' +
          '</div>' +
          '<pre>' + escapeHtml(example.command) + '</pre>' +
        '</article>'
      ).join("");
      document.querySelectorAll("[data-copy]").forEach((button) => {
        button.addEventListener("click", async () => {
          const idx = Number(button.getAttribute("data-copy"));
          await navigator.clipboard.writeText(examples[idx].command);
          button.textContent = "Copied";
          setTimeout(() => button.textContent = "Copy", 1200);
        });
      });
    }

    async function refreshOverview() {
      const data = await fetchJSON("/_stratus/operator/bootstrap");
      const overview = data.overview;
      $("stat-endpoint").textContent = overview.endpoint;
      $("stat-requests").textContent = String(overview.total_requests);
      $("stat-uptime").textContent = String(overview.uptime_seconds) + "s";
      $("stat-log-mode").textContent = "Log " + overview.log_level + " / " + overview.log_format;
      $("stat-data-dir").textContent = overview.data_dir;
      document.title = "Stratus";
    }

    async function refreshActivity() {
      const data = await fetchJSON("/_stratus/operator/activity?limit=12");
      const body = $("activity-body");
      if (!data.items.length) {
        body.innerHTML = '<tr><td class="empty" colspan="5">No operator events yet.</td></tr>';
        return;
      }
      body.innerHTML = data.items.map((item) =>
        '<tr>' +
          '<td>' + escapeHtml(formatTime(item.time)) + '</td>' +
          '<td><strong>' + escapeHtml(item.service || "unknown") + '</strong></td>' +
          '<td>' + escapeHtml(item.operation || "unclassified") + '</td>' +
          '<td><span class="status ' + statusClass(item.status) + '">' + item.status + '</span></td>' +
          '<td>' + escapeHtml(formatDuration(item.duration_ms)) + '</td>' +
        '</tr>'
      ).join("");
    }

    async function refreshErrors() {
      const data = await fetchJSON("/_stratus/operator/errors?limit=8");
      const body = $("errors-body");
      if (!data.items.length) {
        body.innerHTML = '<tr><td class="empty" colspan="4">No failures yet.</td></tr>';
        return;
      }
      body.innerHTML = data.items.map((item) =>
        '<tr>' +
          '<td>' + escapeHtml(formatTime(item.time)) + '</td>' +
          '<td><strong>' + escapeHtml(item.service || "unknown") + '</strong></td>' +
          '<td><span class="status ' + statusClass(item.status) + '">' + item.status + '</span></td>' +
          '<td>' + escapeHtml(item.error_code || item.error_message || "unknown error") + '</td>' +
        '</tr>'
      ).join("");
    }

    async function refreshLogs() {
      const groupSelect = $("group-select");
      const streamSelect = $("stream-select");
      const title = $("log-title");
      const body = $("log-body");
      const previousGroup = groupSelect.value;
      const previousStream = streamSelect.value;

      function renderEmptyLogs(message) {
        title.textContent = "Select a log group and stream";
        body.innerHTML = '<div class="empty">' + escapeHtml(message) + '</div>';
      }

      function renderLogError(message) {
        title.textContent = "Log browser error";
        body.innerHTML = '<div class="empty">Unable to load logs: ' + escapeHtml(message) + '</div>';
      }

      async function renderEvents() {
        const group = groupSelect.value;
        const stream = streamSelect.value;
        title.textContent = group && stream ? group + " / " + stream : "Select a log group and stream";
        if (!group || !stream) {
          renderEmptyLogs("No log events to display.");
          return;
        }
        const eventsData = await fetchJSON("/_stratus/operator/logs/events?group=" + encodeURIComponent(group) + "&stream=" + encodeURIComponent(stream) + "&limit=200");
        if (!eventsData.items.length) {
          body.innerHTML = '<div class="empty">No log events available for this stream yet.</div>';
          return;
        }
        body.innerHTML = eventsData.items.map((event) =>
          '<div class="log-line">' +
            '<div class="muted">' + escapeHtml(formatTime(event.timestamp)) + '</div>' +
            '<div>' + escapeHtml(event.message) + '</div>' +
          '</div>'
        ).join("");
      }

      async function renderStreamsAndEvents() {
        const group = groupSelect.value;
        if (!group) {
          streamSelect.innerHTML = '<option value="">No streams</option>';
          renderEmptyLogs("No log groups available yet.");
          return;
        }

        const streamsData = await fetchJSON("/_stratus/operator/logs/streams?group=" + encodeURIComponent(group));
        if (!streamsData.items.length) {
          streamSelect.innerHTML = '<option value="">No streams</option>';
          title.textContent = group;
          body.innerHTML = '<div class="empty">No log streams available for this group yet.</div>';
          return;
        }

        streamSelect.innerHTML = streamsData.items.map((item) =>
          '<option value="' + escapeHtml(item.stream_name) + '">' + escapeHtml(item.stream_name) + '</option>'
        ).join("");
        if (previousStream && streamsData.items.some((item) => item.stream_name === previousStream)) {
          streamSelect.value = previousStream;
        }
        await renderEvents();
      }

      try {
        const groupsData = await fetchJSON("/_stratus/operator/logs/groups");
        if (!groupsData.items.length) {
          groupSelect.innerHTML = '<option value="">No log groups</option>';
          streamSelect.innerHTML = '<option value="">No streams</option>';
          renderEmptyLogs("No log groups available yet.");
          return;
        }

        groupSelect.innerHTML = groupsData.items.map((item) =>
          '<option value="' + escapeHtml(item.name) + '">' + escapeHtml(item.name) + '</option>'
        ).join("");
        if (previousGroup && groupsData.items.some((item) => item.name === previousGroup)) {
          groupSelect.value = previousGroup;
        }

        await renderStreamsAndEvents();
        groupSelect.onchange = renderStreamsAndEvents;
        streamSelect.onchange = renderEvents;
      } catch (error) {
        groupSelect.innerHTML = '<option value="">No log groups</option>';
        streamSelect.innerHTML = '<option value="">No streams</option>';
        renderLogError(error && error.message ? error.message : "unknown error");
      }
    }

    $("refresh-activity").addEventListener("click", async () => {
      await Promise.all([refreshOverview(), refreshActivity(), refreshErrors()]);
    });
    $("refresh-logs").addEventListener("click", async () => {
      await refreshLogs();
    });

    renderServices();
    renderExamples();
    refreshOverview();
    refreshActivity();
    refreshErrors();
    refreshLogs();
    setInterval(() => {
      refreshOverview().catch(() => {});
      refreshActivity().catch(() => {});
      refreshErrors().catch(() => {});
      refreshLogs().catch(() => {});
    }, 5000);
  </script>
</body>
</html>`
