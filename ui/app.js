/* ─────────────────────────────────────────────────────────────────────────────
   Conduit Dashboard — app.js
   All API calls go to the same origin (Gateway serves both the UI and the API).
───────────────────────────────────────────────────────────────────────────── */

/**
 * @typedef {Object} Connector
 * @property {string} id
 * @property {string} name
 * @property {string} [category]
 * @property {string} [description]
 * @property {string[]} [scopes]
 */

/**
 * @typedef {Object} Instance
 * @property {string} id
 * @property {string} connector_id
 * @property {string} status
 */

/** @typedef {{ abbr: string }} ConnectorMeta */

const API = '';   // Same origin — no base URL prefix needed

// ─── Connector meta (initials shown in the neutral connector tile) ───────────
/** @type {Record<string, ConnectorMeta>} */
const CONNECTOR_META = {
  slack:  { abbr: 'SL' },
  github: { abbr: 'GH' },
  stripe: { abbr: 'ST' },
};

/**
 * @param {string} id
 * @returns {ConnectorMeta}
 */
function connMeta(id) {
  return CONNECTOR_META[id] || { abbr: id ? id.slice(0, 2).toUpperCase() : '??' };
}

/** Escapes text for safe interpolation into innerHTML. @param {unknown} v */
function esc(v) {
  return String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// ─── Navigation ──────────────────────────────────────────────────────────────
const VIEW_TITLES = {
  catalog:   'Integration catalog',
  instances: 'Installed instances',
  execute:   'Execute endpoint',
  metrics:   'Observability',
};

document.querySelectorAll('.nav-item').forEach(link => {
  link.addEventListener('click', e => {
    e.preventDefault();
    switchView(link.dataset.view);
  });
});

/** @param {string} name */
function switchView(name) {
  document.querySelectorAll('.nav-item').forEach(l => {
    l.classList.remove('active');
    l.removeAttribute('aria-current');
  });
  document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
  const navLink = document.getElementById('nav-' + name);
  navLink?.classList.add('active');
  navLink?.setAttribute('aria-current', 'page');
  document.getElementById('view-' + name)?.classList.add('active');
  document.getElementById('view-title').textContent = VIEW_TITLES[name] || name;

  if (name === 'catalog')   loadCatalog();
  if (name === 'instances') loadInstances();
  if (name === 'execute')   loadExecuteSelects();
  if (name === 'metrics')   loadMetricsSummary();
}

// ─── Catalog ─────────────────────────────────────────────────────────────────
async function loadCatalog() {
  const grid = document.getElementById('catalog-grid');
  try {
    /** @type {Connector[]} */
    const connectors = await apiFetch('/v1/connectors');
    grid.innerHTML = '';

    if (!connectors || connectors.length === 0) {
      grid.innerHTML = '<div class="empty-state">No connectors registered.</div>';
      return;
    }

    for (const conn of connectors) {
      grid.appendChild(buildConnectorCard(conn));
    }
  } catch (err) {
    grid.innerHTML = `<div class="empty-state">Failed to load catalog: ${esc(err.message)}</div>`;
  }
}

/** @param {Connector} conn */
function buildConnectorCard(conn) {
  const meta = connMeta(conn.id);
  const card = el('div', 'connector-card');

  const scopes = (conn.scopes || []).slice(0, 4)
    .map(s => `<span class="scope">${esc(s)}</span>`)
    .join('');

  card.innerHTML = `
    <div class="card-header">
      <span class="card-icon" aria-hidden="true">${esc(meta.abbr)}</span>
      <div class="card-title">
        <span class="card-name">${esc(conn.name)}</span>
        <span class="card-category">${esc(conn.category || 'Integration')}</span>
      </div>
    </div>
    <div class="card-desc">${esc(conn.description)}</div>
    <div class="card-footer">
      ${scopes ? `<div class="card-scopes">${scopes}</div>` : '<span class="card-auth">API key</span>'}
    </div>
  `;

  const installBtn = el('button', 'btn btn-secondary btn-sm');
  installBtn.textContent = 'Install';
  installBtn.setAttribute('aria-label', `Install ${conn.name}`);
  installBtn.onclick = () => openInstallModal(conn);
  card.querySelector('.card-footer').appendChild(installBtn);

  return card;
}

// ─── Install Modal ────────────────────────────────────────────────────────────
/** @type {Connector | null} */
let _installing = null;
/** @type {HTMLElement | null} Element focused before the modal opened, restored on close. */
let _preModalFocus = null;

/** @param {Connector} conn */
function openInstallModal(conn) {
  _installing = conn;
  _preModalFocus = /** @type {HTMLElement} */ (document.activeElement);
  const modal = document.getElementById('install-modal');
  const body  = document.getElementById('modal-body');
  document.getElementById('modal-title').textContent = `Install ${conn.name}`;

  const isAPIKey = conn.scopes?.length === 0 || conn.id === 'stripe';

  if (isAPIKey) {
    body.innerHTML = `
      <p><strong>${conn.name}</strong> uses an <strong>API key</strong> for authentication.</p>
      <div class="form-group">
        <label for="modal-api-key">API key</label>
        <input id="modal-api-key" class="form-input" type="password" placeholder="sk_live_..." autocomplete="off" />
      </div>
      <div class="modal-actions">
        <button class="btn btn-secondary" onclick="closeInstallModal()">Cancel</button>
        <button class="btn btn-primary" onclick="installAPIKey('${conn.id}')">Install</button>
      </div>
    `;
  } else {
    body.innerHTML = `
      <p><strong>${conn.name}</strong> uses <strong>OAuth 2.0</strong>. You'll be redirected to the provider to authorise access.</p>
      <p>Requested scopes</p>
      <div class="modal-scopes">${(conn.scopes || []).map(s => `<span class="scope">${esc(s)}</span>`).join('')}</div>
      <div class="modal-actions">
        <button class="btn btn-secondary" onclick="closeInstallModal()">Cancel</button>
        <button class="btn btn-primary" onclick="installOAuth('${conn.id}')">Authorise &rarr;</button>
      </div>
    `;
  }

  modal.style.display = 'flex';
  // Focus the first interactive control inside the modal for keyboard users.
  modal.querySelector('.modal input, .modal button')?.focus();
}

function closeInstallModal() {
  document.getElementById('install-modal').style.display = 'none';
  _installing = null;
  _preModalFocus?.focus();
  _preModalFocus = null;
}

const installModalEl = document.getElementById('install-modal');

// Close modal on backdrop click
installModalEl.addEventListener('click', e => {
  if (e.target === e.currentTarget) closeInstallModal();
});

// Escape-to-close and a focus trap so Tab cannot leave the dialog while it's open.
installModalEl.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    closeInstallModal();
    return;
  }
  if (e.key !== 'Tab') return;

  const focusable = Array.from(installModalEl.querySelectorAll('button, input, select, textarea, a[href]'))
    .filter(node => !node.disabled && node.offsetParent !== null);
  if (focusable.length === 0) return;

  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
});

async function installOAuth(connectorID) {
  try {
    const result = await apiFetch(`/v1/connectors/${connectorID}/install`, { method: 'POST' });
    closeInstallModal();
    if (result.redirect_url) {
      // Open OAuth in same tab — the gateway returns an HTML success page
      window.location.href = result.redirect_url;
    }
  } catch (err) {
    toast('Install failed: ' + err.message, 'error');
  }
}

async function installAPIKey(connectorID) {
  const key = document.getElementById('modal-api-key')?.value?.trim();
  if (!key) {
    toast('API key is required', 'error');
    return;
  }
  try {
    const result = await apiFetch(`/v1/connectors/${connectorID}/install`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ api_key: key }),
    });
    closeInstallModal();
    toast(`${connectorID} installed as ${result.instance_id}`, 'success');
    switchView('instances');
  } catch (err) {
    toast('Install failed: ' + err.message, 'error');
  }
}

// ─── Instances ────────────────────────────────────────────────────────────────
async function loadInstances() {
  const list = document.getElementById('instances-list');
  list.innerHTML = '<div class="loading-state"><span class="spinner"></span> Loading instances…</div>';
  try {
    /** @type {Instance[]} */
    const instances = await apiFetch('/v1/instances');
    updateBadge(instances?.length || 0);
    document.getElementById('metric-instances-count').textContent = instances?.length ?? 0;

    if (!instances || instances.length === 0) {
      list.innerHTML = '<div class="empty-state">No instances installed yet. Install a connector from the Catalog.</div>';
      return;
    }

    list.innerHTML = '';
    for (const inst of instances) {
      list.appendChild(buildInstanceRow(inst));
    }
  } catch (err) {
    list.innerHTML = `<div class="empty-state">Failed to load instances: ${esc(err.message)}</div>`;
  }
}

/** @param {Instance} inst */
function buildInstanceRow(inst) {
  const meta = connMeta(inst.connector_id);
  const statusClass = `status-${esc(inst.status)}`;

  const row = el('div', 'instance-row');
  row.innerHTML = `
    <span class="instance-icon" aria-hidden="true">${esc(meta.abbr)}</span>
    <div class="instance-info">
      <div class="instance-connector">${esc(inst.connector_id)}</div>
      <div class="instance-id">${esc(inst.id)}</div>
    </div>
    <span class="status ${statusClass}"><span class="status-dot"></span>${esc(inst.status)}</span>
    <div class="instance-actions">
      <button class="btn btn-secondary btn-sm" onclick="quickExecute('${inst.id}', '${inst.connector_id}')">Execute</button>
      <button class="btn btn-danger btn-sm" onclick="deleteInstance('${inst.id}')">Remove</button>
    </div>
  `;
  return row;
}

function quickExecute(instanceID, connectorID) {
  switchView('execute');
  setTimeout(() => {
    const sel = document.getElementById('exec-instance');
    if (sel) {
      sel.value = instanceID;
      sel.dispatchEvent(new Event('change'));
    }
  }, 100);
}

async function deleteInstance(id) {
  if (!confirm(`Remove instance ${id}?`)) return;
  try {
    await apiFetch(`/v1/instances/${id}`, { method: 'DELETE' });
    toast('Instance removed', 'success');
    loadInstances();
  } catch (err) {
    toast('Failed to remove: ' + err.message, 'error');
  }
}

function updateBadge(count) {
  const badge = document.getElementById('instances-badge');
  badge.textContent = count;
  badge.style.display = count > 0 ? '' : 'none';
}

// ─── Execute Panel ────────────────────────────────────────────────────────────

// Endpoint metadata per connector
const ENDPOINTS = {
  slack:  ['send_message'],
  github: ['create_issue', 'list_issues'],
  stripe: ['list_charges'],
};

// Default bodies for each endpoint
const DEFAULT_BODIES = {
  send_message:  JSON.stringify({ channel: 'C_GENERAL', text: 'Hello from Conduit' }, null, 2),
  create_issue:  JSON.stringify({ owner: 'my-org', repo: 'my-repo', title: 'New issue from Conduit', body: 'Auto-created.' }, null, 2),
  list_issues:   JSON.stringify({}, null, 2),
  list_charges:  JSON.stringify({}, null, 2),
};

async function loadExecuteSelects() {
  const instSel = document.getElementById('exec-instance');
  const epSel   = document.getElementById('exec-endpoint');

  try {
    const instances = await apiFetch('/v1/instances');
    instSel.innerHTML = instances?.length
      ? instances.map(i => `<option value="${i.id}" data-connector="${i.connector_id}">${i.connector_id} · ${i.id}</option>`).join('')
      : '<option value="">No instances available</option>';

    instSel.onchange = () => {
      const opt = instSel.selectedOptions[0];
      const connID = opt?.dataset.connector || '';
      const eps = ENDPOINTS[connID] || [];
      epSel.innerHTML = eps.map(e => `<option value="${e}">${e}</option>`).join('');
      epSel.dispatchEvent(new Event('change'));
    };

    epSel.onchange = () => {
      const ep = epSel.value;
      if (DEFAULT_BODIES[ep]) {
        document.getElementById('exec-body').value = DEFAULT_BODIES[ep];
      }
    };

    instSel.dispatchEvent(new Event('change'));
  } catch (err) {
    instSel.innerHTML = '<option>Error loading instances</option>';
  }
}

async function executeEndpoint() {
  const instanceID   = document.getElementById('exec-instance').value;
  const endpointName = document.getElementById('exec-endpoint').value;
  const bodyText     = document.getElementById('exec-body').value.trim();

  if (!instanceID || !endpointName) {
    toast('Select an instance and endpoint first', 'error');
    return;
  }

  const btn = document.getElementById('exec-btn');
  const btnLabel = document.getElementById('exec-btn-label');
  btnLabel.textContent = 'Executing…';
  btn.disabled = true;

  const responseStatus = document.getElementById('response-status');
  const responseBody   = document.getElementById('response-body');

  try {
    const url = `/v1/instances/${instanceID}/endpoints/${endpointName}`;
    const raw = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: bodyText || '{}',
    });

    const text = await raw.text();
    let formatted;
    try {
      formatted = JSON.stringify(JSON.parse(text), null, 2);
    } catch {
      formatted = text;
    }

    responseStatus.textContent = `${raw.status} ${raw.statusText}`;
    responseStatus.className = 'response-status ' + (raw.ok ? 'ok' : 'err');
    responseBody.classList.remove('is-empty');
    responseBody.innerHTML = highlightJSON(formatted);

    if (raw.ok) {
      toast(`${endpointName} executed successfully (${raw.status})`, 'success');
    } else {
      toast(`Endpoint returned ${raw.status}`, 'error');
    }
  } catch (err) {
    toast('Request failed: ' + err.message, 'error');
  } finally {
    btnLabel.textContent = 'Execute';
    btn.disabled = false;
  }
}

/**
 * Escapes a formatted JSON string and wraps keys, strings, numbers and
 * literals in spans for the response viewer. Non-JSON text is only escaped.
 * @param {string} text
 */
function highlightJSON(text) {
  return esc(text).replace(
    /(&quot;(?:[^&]|&(?!quot;))*?&quot;)(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|([{}\[\],])/g,
    (m, str, colon, lit, num, punc) => {
      if (str) return colon ? `<span class="j-key">${str}</span><span class="j-punc">${colon}</span>` : `<span class="j-str">${str}</span>`;
      if (lit) return `<span class="j-lit">${lit}</span>`;
      if (num) return `<span class="j-num">${num}</span>`;
      return `<span class="j-punc">${punc}</span>`;
    });
}

// ─── Metrics Summary ──────────────────────────────────────────────────────────
async function loadMetricsSummary() {
  try {
    const instances = await apiFetch('/v1/instances');
    document.getElementById('metric-instances-count').textContent = instances?.length ?? 0;
  } catch {}
}

// ─── Utilities ────────────────────────────────────────────────────────────────
function el(tag, className) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  return e;
}

async function apiFetch(path, options = {}) {
  const resp = await fetch(API + path, options);
  const text = await resp.text();
  let data;
  try { data = JSON.parse(text); } catch { data = text; }
  if (!resp.ok) {
    const msg = (typeof data === 'object' && data?.error) ? data.error : String(data);
    throw new Error(msg || `HTTP ${resp.status}`);
  }
  return data;
}

const TOAST_ICONS = {
  success: `<svg class="toast-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="8" r="6.5"/><polyline points="5,8.5 7,10.5 11,6"/></svg>`,
  error:   `<svg class="toast-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="8" r="6.5"/><line x1="5.5" y1="5.5" x2="10.5" y2="10.5"/><line x1="10.5" y1="5.5" x2="5.5" y2="10.5"/></svg>`,
  info:    `<svg class="toast-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="8" r="6.5"/><line x1="8" y1="7" x2="8" y2="11"/><circle cx="8" cy="5" r="0.5" fill="currentColor"/></svg>`,
};

function toast(message, type = 'info') {
  const container = document.getElementById('toast-container');
  const t = el('div', `toast toast-${type}`);
  t.innerHTML = TOAST_ICONS[type] || TOAST_ICONS.info;
  const text = document.createElement('span');
  text.textContent = message;
  t.appendChild(text);
  container.appendChild(t);
  setTimeout(() => t.remove(), 4000);
}

/** @param {boolean} online */
function setGatewayStatus(online) {
  document.getElementById('gateway-status').className = `gateway-status ${online ? 'online' : 'offline'}`;
  document.getElementById('gateway-status-text').textContent = online ? 'Gateway online' : 'Gateway unreachable';
}

// ─── Init ─────────────────────────────────────────────────────────────────────
(async function init() {
  // Load catalog by default
  loadCatalog();

  // Warm up the badge; this call doubles as the gateway health check.
  try {
    const instances = await apiFetch('/v1/instances');
    updateBadge(instances?.length || 0);
    setGatewayStatus(true);
  } catch {
    setGatewayStatus(false);
  }
})();
