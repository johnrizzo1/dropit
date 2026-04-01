// === Shared utilities for Reverser ===

// Markdown rendering via marked.js (falls back to escaped text if not loaded)
function renderMd(text) {
  if (typeof marked !== 'undefined') {
    return marked.parse(text);
  }
  return `<pre>${esc(text)}</pre>`;
}

function esc(text) {
  const div = document.createElement('div');
  div.textContent = text;
  return div.innerHTML;
}

function parseBinaryName(dirName) {
  const idx = dirName.indexOf('_');
  if (idx > 0 && /^\d+$/.test(dirName.substring(0, idx))) {
    return dirName.substring(idx + 1);
  }
  return dirName;
}

function parseTimestamp(dirName) {
  const idx = dirName.indexOf('_');
  if (idx > 0) {
    const ts = dirName.substring(0, idx);
    if (/^\d{10,}$/.test(ts)) {
      const ms = parseInt(ts) / 1e6;
      const d = new Date(ms);
      if (!isNaN(d.getTime())) return d.toLocaleString();
    }
  }
  return null;
}

function formatSize(bytes) {
  if (bytes < 1024) return bytes + 'B';
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + 'K';
  return (bytes / (1024 * 1024)).toFixed(1) + 'M';
}

function cleanToolName(name) {
  return name.replace(/^mcp__re__/, '');
}

function formatToolArgs(input) {
  if (!input) return '';
  const parts = [];
  for (const [k, v] of Object.entries(input)) {
    if (k === 'path' && v === '/tmp/target') continue;
    const val = typeof v === 'string' ? v : JSON.stringify(v);
    parts.push(val.length > 60 ? `${k}=${val.substring(0, 57)}...` : `${k}=${val}`);
  }
  return parts.join(' ');
}

async function fetchSessionEvents(key) {
  try {
    const resp = await fetch(`/api/runs/_/file?key=${encodeURIComponent(key)}`);
    const text = await resp.text();
    return text.trim().split('\n').filter(Boolean).map(line => {
      try { return JSON.parse(line); }
      catch { return { type: 'parse_error', raw: line }; }
    });
  } catch { return []; }
}

function buildTerminalHTML(events, titleText) {
  let lines = '';
  let lastTurn = 0;

  for (const e of events) {
    switch (e.type) {
      case 'session_start':
        lines += `<div class="t-line t-prompt">reverser ${esc(e.mode || 'analyze')} /tmp/target --budget ${esc(String(e.budget || '?'))}</div>`;
        break;
      case 'turn':
        if (e.turn && e.turn > 1 && e.turn > lastTurn + 1) lines += `<hr class="t-separator">`;
        lastTurn = e.turn || 0;
        if (e.turn && e.turn % 10 === 0) lines += `<div class="t-line t-turn-marker">--- turn ${e.turn} ---</div>`;
        break;
      case 'thinking':
        if (e.text) lines += `<div class="t-line t-thinking">${esc(e.text)}</div>`;
        break;
      case 'text':
        if (e.text) lines += `<div class="t-line t-text">${renderMd(e.text)}</div>`;
        break;
      case 'tool_call': {
        const args = e.input ? formatToolArgs(e.input) : '';
        lines += `<div class="t-line t-tool"><span class="tool-name">${esc(cleanToolName(e.name || '?'))}</span> <span class="tool-args">${esc(args)}</span></div>`;
        break;
      }
      case 'tool_result':
        if (e.content) {
          const trunc = e.content.length > 500;
          lines += `<div class="t-line t-result${trunc ? ' truncated' : ''}" onclick="this.classList.toggle('expanded');this.classList.remove('truncated')">${esc(e.content)}</div>`;
        }
        break;
      case 'error':
        lines += `<div class="t-line t-error">${esc(e.text || e.message || JSON.stringify(e))}</div>`;
        break;
    }
  }

  return `<div class="terminal">
    <div class="terminal-bar">
      <span class="terminal-dot red"></span>
      <span class="terminal-dot yellow"></span>
      <span class="terminal-dot green"></span>
      <span class="terminal-title">${esc(titleText)}</span>
    </div>
    <div class="terminal-body">${lines}</div>
  </div>`;
}

function buildPillsHTML(events) {
  const start = events.find(e => e.type === 'session_start');
  if (!start) return '';
  const pills = [];
  if (start.mode) pills.push(['Mode', start.mode]);
  if (start.budget) pills.push(['Budget', `$${start.budget}`]);
  if (start.ts) pills.push(['Started', new Date(start.ts).toLocaleString()]);
  const turns = events.filter(e => e.type === 'turn').length;
  if (turns > 0) pills.push(['Turns', turns]);
  const tools = events.filter(e => e.type === 'tool_call').length;
  if (tools > 0) pills.push(['Tool Calls', tools]);
  return pills.map(([l, v]) =>
    `<span class="pill"><span class="pill-label">${esc(l)}</span><span class="pill-value">${esc(String(v))}</span></span>`
  ).join('');
}

function buildResultsHighlightHTML(events) {
  const texts = events.filter(e => e.type === 'text' && e.text && e.text.length > 50);
  const lastText = texts.length > 0 ? texts[texts.length - 1].text : null;
  if (lastText) {
    return `<div class="results-highlight"><h3>Agent Findings</h3><div class="md-content">${renderMd(lastText)}</div></div>`;
  }
  return `<div class="results-highlight"><h3>Agent Findings</h3><div class="no-results">No final results yet.</div></div>`;
}

function buildFilesHTML(files) {
  if (!files || files.length === 0) return '';
  let html = '<div class="files-section"><h3>Files</h3><div class="file-chips">';
  for (const f of files) {
    html += `<span class="file-chip" onclick="viewFile(this, '${esc(f.key)}')">${esc(f.name)}<span class="chip-size">${formatSize(f.size)}</span></span>`;
  }
  html += '</div><div id="file-viewer-terminal" class="file-terminal"></div></div>';
  return html;
}

async function viewFile(chip, key) {
  document.querySelectorAll('.file-chip').forEach(c => c.classList.remove('active'));
  chip.classList.add('active');

  const viewer = document.getElementById('file-viewer-terminal');
  viewer.className = 'file-terminal visible';
  viewer.innerHTML = `<div class="terminal">
    <div class="terminal-bar">
      <span class="terminal-dot red"></span><span class="terminal-dot yellow"></span><span class="terminal-dot green"></span>
      <span class="terminal-title">cat ${esc(key.split('/').pop())}</span>
    </div>
    <div class="terminal-body" style="max-height:40vh"><div class="t-line" style="color:#555">Loading...</div></div>
  </div>`;

  try {
    const resp = await fetch(`/api/runs/_/file?key=${encodeURIComponent(key)}`);
    const text = await resp.text();
    const body = viewer.querySelector('.terminal-body');
    if (key.endsWith('.md')) {
      body.innerHTML = `<div class="md-content">${renderMd(text)}</div>`;
    } else if (key.endsWith('.jsonl')) {
      const pretty = text.trim().split('\n').filter(Boolean).map(line => {
        try { return JSON.stringify(JSON.parse(line), null, 2); } catch { return line; }
      }).join('\n');
      body.innerHTML = `<pre style="margin:0;color:#c8d8e8;white-space:pre-wrap;word-break:break-word">${esc(pretty)}</pre>`;
    } else {
      body.innerHTML = `<pre style="margin:0;color:#c8d8e8;white-space:pre-wrap;word-break:break-word">${esc(text)}</pre>`;
    }
  } catch {
    viewer.querySelector('.terminal-body').innerHTML = '<div class="t-line t-error">Failed to load file.</div>';
  }
}

// === Upload modal with Turnstile CAPTCHA ===
let captchaSiteKey = '';
let captchaReady = false;
let captchaWidgetId = null;

function openUploadModal() {
  document.getElementById('upload-modal').classList.add('visible');
  // Render captcha widget if we have a key and haven't rendered yet
  if (captchaSiteKey && !captchaWidgetId && typeof turnstile !== 'undefined') {
    renderCaptchaWidget();
  }
}

function closeUploadModal() {
  document.getElementById('upload-modal').classList.remove('visible');
}

function renderCaptchaWidget() {
  const container = document.getElementById('captcha-container');
  if (!container || captchaWidgetId !== null) return;
  captchaWidgetId = turnstile.render(container, {
    sitekey: captchaSiteKey,
    theme: 'dark',
    callback: (token) => { captchaReady = true; },
    'expired-callback': () => { captchaReady = false; },
    'error-callback': () => { captchaReady = false; },
  });
}

async function initUploadModal() {
  const modal = document.getElementById('upload-modal');
  const dropZone = document.getElementById('drop-zone');
  const fileInput = document.getElementById('file-input');

  // Load captcha site key
  try {
    const resp = await fetch('/api/captcha-key');
    const data = await resp.json();
    if (data.siteKey) {
      captchaSiteKey = data.siteKey;
      document.getElementById('captcha-container').style.display = 'flex';
    } else {
      // No captcha configured, allow uploads directly
      captchaReady = true;
    }
  } catch {
    captchaReady = true; // Fail open if can't reach API
  }

  modal.addEventListener('click', (e) => {
    if (e.target === modal) closeUploadModal();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closeUploadModal();
  });

  dropZone.addEventListener('click', () => fileInput.click());
  dropZone.addEventListener('dragover', (e) => {
    e.preventDefault();
    dropZone.classList.add('dragover');
  });
  dropZone.addEventListener('dragleave', () => dropZone.classList.remove('dragover'));
  dropZone.addEventListener('drop', (e) => {
    e.preventDefault();
    dropZone.classList.remove('dragover');
    for (const file of e.dataTransfer.files) uploadFile(file);
  });

  fileInput.addEventListener('change', () => {
    for (const file of fileInput.files) uploadFile(file);
    fileInput.value = '';
  });
}

function getCaptchaToken() {
  if (!captchaSiteKey) return ''; // No captcha configured
  if (captchaWidgetId !== null && typeof turnstile !== 'undefined') {
    return turnstile.getResponse(captchaWidgetId) || '';
  }
  return '';
}

function resetCaptcha() {
  if (captchaWidgetId !== null && typeof turnstile !== 'undefined') {
    turnstile.reset(captchaWidgetId);
    captchaReady = false;
  }
}

function uploadFile(file) {
  if (captchaSiteKey && !captchaReady) {
    alert('Please complete the CAPTCHA verification first.');
    return;
  }

  const area = document.getElementById('upload-progress');
  const item = document.createElement('div');
  item.className = 'upload-item';
  item.innerHTML = `
    <div>
      <div class="name">${esc(file.name)}</div>
      <div class="progress-bar"><div class="fill" style="width:0%"></div></div>
    </div>
    <div class="status status-uploading">Uploading...</div>
  `;
  area.prepend(item);

  const fill = item.querySelector('.fill');
  const status = item.querySelector('.status');
  const fd = new FormData();
  fd.append('file', file);
  fd.append('cf-turnstile-response', getCaptchaToken());

  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/upload');
  xhr.upload.addEventListener('progress', (e) => {
    if (e.lengthComputable) fill.style.width = Math.round((e.loaded / e.total) * 100) + '%';
  });
  xhr.addEventListener('load', () => {
    try {
      const resp = JSON.parse(xhr.responseText);
      if (resp.success) {
        status.textContent = 'Queued';
        status.className = 'status status-done';
        fill.style.width = '100%';
        fill.style.background = '#4ecca3';
      } else {
        status.textContent = resp.error || 'Error';
        status.className = 'status status-error';
        fill.style.background = '#ff6b6b';
      }
    } catch {
      status.textContent = 'Error';
      status.className = 'status status-error';
    }
    resetCaptcha(); // Reset for next upload
  });
  xhr.addEventListener('error', () => {
    status.textContent = 'Network error';
    status.className = 'status status-error';
    fill.style.background = '#ff6b6b';
    resetCaptcha();
  });
  xhr.send(fd);
}
