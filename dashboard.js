let ws = null;
let serverMode = false;
let activeAgentId = null;
let controlledAgentId = null;
let transportOrder = 'ws,quic';
let startTime = Date.now();
let qualityPanelOpen = false;
let metricsPanelOpen = false;
let agentsPanelOpen = false;
let settingsLoggedIn = false;
let authenticated = false;
let viewMode = 'grid';
let remoteControlEnabled = false;
const agentCells = {};
let knownAgentIds = new Set();
let hiddenAgents = new Set();
let allAuditEntries = []; // populated by background /api/audit poll; used by XLSX report
let selectedAgents = new Set();
let gridThrottle = {};

// WebRTC
let webrtcPC = null;
let webrtcDC = null;
let webrtcActive = false;
let webrtcPendingICE = [];

// ── Panel Toggles ──
function toggleQualityPanel() {
  qualityPanelOpen = !qualityPanelOpen;
  closeOtherPanels('quality');
  document.getElementById('quality-panel').classList.toggle('open', qualityPanelOpen);
}
function toggleMetricsPanel() {
  metricsPanelOpen = !metricsPanelOpen;
  closeOtherPanels('metrics');
  document.getElementById('metrics-panel').classList.toggle('open', metricsPanelOpen);
}
function toggleAgentsPanel() {
  agentsPanelOpen = !agentsPanelOpen;
  closeOtherPanels('agents');
  document.getElementById('agents-panel').classList.toggle('open', agentsPanelOpen);
  if (agentsPanelOpen) refreshAgents();
}
function closeOtherPanels(keep) {
  if (keep !== 'quality') { qualityPanelOpen = false; document.getElementById('quality-panel').classList.remove('open'); }
  if (keep !== 'metrics') { metricsPanelOpen = false; document.getElementById('metrics-panel').classList.remove('open'); }
  if (keep !== 'agents') { agentsPanelOpen = false; document.getElementById('agents-panel').classList.remove('open'); }
}
document.addEventListener('click', (e) => {
  if (qualityPanelOpen && !e.target.closest('#quality-panel') && !e.target.closest('[onclick*="toggleQualityPanel"]')) { qualityPanelOpen = false; document.getElementById('quality-panel').classList.remove('open'); }
  if (metricsPanelOpen && !e.target.closest('#metrics-panel') && !e.target.closest('[onclick*="toggleMetricsPanel"]')) { metricsPanelOpen = false; document.getElementById('metrics-panel').classList.remove('open'); }
  if (agentsPanelOpen && !e.target.closest('#agents-panel') && !e.target.closest('[onclick*="toggleAgentsPanel"]')) { agentsPanelOpen = false; document.getElementById('agents-panel').classList.remove('open'); }
});

// ── WebRTC ──
function initWebRTC() {
  if (webrtcPC) { try { webrtcPC.close(); } catch(_) {} webrtcPC = null; webrtcDC = null; webrtcActive = false; }
  try {
    const cfg = { iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] };
    webrtcPC = new RTCPeerConnection(cfg);
    webrtcDC = webrtcPC.createDataChannel('frames');
    webrtcDC.onopen = () => { webrtcActive = true; webrtcPendingICE = []; };
    webrtcDC.onclose = () => { webrtcActive = false; };
    webrtcDC.onmessage = (e) => {
      try {
        const d = JSON.parse(e.data);
        if (d.type === 'frame' && d.data) handleFrame(d);
      } catch(_) {}
    };
    webrtcPC.onicecandidate = (e) => {
      if (e.candidate && ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'webrtc_ice', candidate: JSON.stringify(e.candidate.toJSON()) }));
      }
    };
    webrtcPC.oniceconnectionstatechange = () => {
      if (webrtcPC.iceConnectionState === 'failed' || webrtcPC.iceConnectionState === 'disconnected') {
        webrtcActive = false;
      }
    };
    webrtcPC.createOffer().then(offer => {
      webrtcPC.setLocalDescription(offer);
      ws.send(JSON.stringify({ type: 'webrtc_offer', sdp: offer.sdp }));
    }).catch(() => { webrtcActive = false; });
  } catch(_) { webrtcActive = false; }
}

function handleWebRTCAnswer(sdp) {
  if (!webrtcPC) return;
  webrtcPC.setRemoteDescription(new RTCSessionDescription({ type: 'answer', sdp })).catch(() => {});
}

function handleWebRTCICE(candidateStr) {
  if (!webrtcPC) return;
  try {
    const c = JSON.parse(candidateStr);
    webrtcPC.addIceCandidate(new RTCIceCandidate(c)).catch(() => {});
  } catch(_) {}
}

// ── WebSocket ──
function connect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = `${proto}//${location.host}/ws`;
  if (ws) try { ws.close(); } catch(_) {}
  ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';
  ws.onopen = () => {
    document.getElementById('status-dot').className = 'online';
    resetFrameTimer();
    ws.send(JSON.stringify({type:'hello', agent: false, transports:['ws']}));
  };
  ws.onclose = () => {
    document.getElementById('status-dot').className = '';
    webrtcActive = false;
    setTimeout(connect, 2000);
  };
  ws.onmessage = (e) => {
    if (typeof e.data === 'string') {
      // Text message (control, status, etc.)
      try { handleMessage(JSON.parse(e.data)); } catch(err) {}
    } else if (e.data instanceof ArrayBuffer) {
      // Binary message: [4 bytes agentId length][agentId bytes][JPEG bytes]
      handleBinaryFrame(e.data);
    }
  };
}

// Periodic re-render of the main canvas: handles cases where a render
// was dropped or the user clicked Focus before any frame arrived.
function startMainCanvasRefresher() {
  if (window._mainCanvasRefresher) return;
  window._mainCanvasRefresher = setInterval(() => {
    if (viewMode !== 'single' || !controlledAgentId) return;
    const rec = agentCells[controlledAgentId];
    if (!rec) return;
    const payload = rec.lastPayload || (rec.lastB64 ? { data: rec.lastB64 } : null);
    if (payload) {
      requestAnimationFrame(() => renderToMainCanvas(controlledAgentId, payload));
    }
  }, 2000);
}

function handleBinaryFrame(buffer) {
  const view = new DataView(buffer);
  const agentIdLen = view.getUint32(0);
  const agentIdBytes = new Uint8Array(buffer, 4, agentIdLen);
  const agentId = new TextDecoder().decode(agentIdBytes);
  const jpegData = new Uint8Array(buffer, 4 + agentIdLen);
  // Always generate base64 string synchronously — this is the reliable path
  // that the main canvas (Focus) uses. The grid cells additionally benefit
  // from createImageBitmap for faster rendering of small thumbnails.
  const b64 = bytesToBase64(jpegData);
  if (typeof createImageBitmap === 'function') {
    const blob = new Blob([jpegData], { type: 'image/jpeg' });
    createImageBitmap(blob).then(bmp => {
      // Send BOTH bitmap (for grid cells) and base64 data (for main canvas)
      handleFrame({type:'frame', agentId: agentId, bitmap: bmp, data: b64});
    }).catch(() => {
      // createImageBitmap failed — base64-only path
      handleFrame({type:'frame', agentId: agentId, data: b64});
    });
  } else {
    // Very old browser without createImageBitmap — base64-only
    handleFrame({type:'frame', agentId: agentId, data: b64});
  }
}

function bytesToBase64(jpegData) {
  const CHUNK = 0x8000;
  let binary = '';
  for (let i = 0; i < jpegData.length; i += CHUNK) {
    binary += String.fromCharCode.apply(null, jpegData.subarray(i, i + CHUNK));
  }
  return btoa(binary);
}

function handleFrame(d) {
  const aid = d.agentId || 'default';
  if (d.agentId) {
    activeAgentId = d.agentId;
  }
  // Pass full payload: may be {bitmap} (fast path) or {data: b64} (fallback)
  renderFrame(aid, d);
}

function handleMessage(d) {
  if (d.type === 'frame' && d.data) {
    handleFrame(d);
    return;
  }
  if (d.type === 'status' || d.frames_captured !== undefined) { updateMetrics(d); return; }
  if (Array.isArray(d)) {
    d.forEach(id => { knownAgentIds.add(id); });
    syncAgentListFromKnown();
    renderAgentSelect(d);
    return;
  }
  if (d.type === 'share_link') {
    document.getElementById('share-link-output').value = d.url || 'Error generating link';
    toggleModal('share-modal');
    return;
  }
  if (d.type === 'tunnel_status') { return; }
  if (d.type === 'promoted') {
    serverMode = true;
    document.getElementById('mode-badge').textContent = 'SERVER';
    return;
  }
  if (d.type === 'webrtc_answer') {
    handleWebRTCAnswer(d.sdp);
    return;
  }
  if (d.type === 'webrtc_ice') {
    handleWebRTCICE(d.candidate);
    return;
  }
}


// ── View modes ──

// Focus the dashboard on THIS machine (the one running PunMonitor that the
// dashboard is connected to). Uses myAgentId captured from /api/system-info.
// Falls back to fetching it on demand if myAgentId isn't set yet.
function focusThisMachine() {
  if (!myAgentId) {
    // myAgentId not loaded yet — fetch it and retry
    fetch('/api/system-info').then(r=>r.json()).then(info => {
      if (info.agent_id) {
        myAgentId = info.agent_id;
        const idEl = document.getElementById('my-agent-id');
        if (idEl) idEl.textContent = myAgentId;
        doFocusThisMachine();
      }
    }).catch(()=>{
      // Couldn't reach the server — fall back to whatever the first agent is
      const target = controlledAgentId || activeAgentId || [...knownAgentIds][0];
      if (target) {
        setControlledAgent(target);
        setViewMode('single');
      }
    });
    return;
  }
  doFocusThisMachine();
}
function doFocusThisMachine() {
  if (!myAgentId) return;
  // Ensure the cell exists in the agents list so the placeholder can show
  if (!knownAgentIds.has(myAgentId)) {
    knownAgentIds.add(myAgentId);
    if (typeof refreshAgents === 'function') refreshAgents();
  }
  setControlledAgent(myAgentId);
  setViewMode('single');
  // If we already have a frame for this machine, render it after the layout
  // has settled. Defer to next animation frame so #screen-container has
  // non-zero dimensions after switching to single mode.
  if (agentCells[myAgentId]) {
    const rec = agentCells[myAgentId];
    const payload = rec.lastPayload || (rec.lastB64 ? { data: rec.lastB64 } : null);
    if (payload) requestAnimationFrame(() => renderToMainCanvas(myAgentId, payload));
  } else {
    // No frame yet — show the placeholder
    const placeholder = document.getElementById('screen-placeholder');
    if (placeholder) {
      placeholder.style.display = 'flex';
      const txt = placeholder.querySelector('div:nth-child(2)');
      if (txt) txt.textContent = 'Waiting for first frame from this machine (' + myAgentId + ')...';
    }
  }
}

function setViewMode(mode) {
  viewMode = mode;
  try { localStorage.setItem('pm_viewMode', mode); } catch(_){}
  const main = document.getElementById('main');
  main.classList.toggle('grid-mode', mode === 'grid');
  main.classList.toggle('single-mode', mode === 'single');
  document.getElementById('btn-grid-view').classList.toggle('active', mode === 'grid');
  document.getElementById('btn-single-view').classList.toggle('active', mode === 'single');
  if (mode === 'single') {
    const target = controlledAgentId || activeAgentId || [...knownAgentIds][0];
    if (target) setControlledAgent(target, false);
    updateControlBar();
    if (agentCells[target]) {
      const rec = agentCells[target];
      const payload = rec.lastPayload || (rec.lastB64 ? { data: rec.lastB64 } : null);
      if (payload) requestAnimationFrame(() => renderToMainCanvas(target, payload));
    }
  } else {
    updateControlBar();
    updateCellHighlights();
  }
}

function setControlledAgent(id, sendSelect) {
  controlledAgentId = id;
  try { if (id) localStorage.setItem('pm_controlledAgent', id); } catch(_){}
  if (!id) { updateControlBar(); updateCellHighlights(); return; }
  activeAgentId = id;
  if (sendSelect !== false && ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'select_agent', agentId: id }));
  }
  updateControlBar();
  updateCellHighlights();
}

function enableRemoteControl() {
  if (!authenticated) {
    document.getElementById('login-modal').dataset.return = 'control';
    toggleModal('login-modal');
    return;
  }
  if (!controlledAgentId) { alert('Select an agent first (click a grid tile)'); return; }
  remoteControlEnabled = true;
  updateControlBar();
  updateCellHighlights();
}

function disableRemoteControl() {
  remoteControlEnabled = false;
  updateControlBar();
  updateCellHighlights();
}

function updateControlBar() {
  const label = document.getElementById('control-agent-label');
  const hint = document.getElementById('control-status');
  const takeBtn = document.getElementById('btn-take-control');
  const releaseBtn = document.getElementById('btn-release-control');
  const bar = document.getElementById('control-bar');
  if (label) label.textContent = controlledAgentId || '—';
  if (hint) {
    if (!authenticated) hint.textContent = 'Login as admin, then Take Control';
    else if (!remoteControlEnabled) hint.textContent = 'View only — click Take Control to send mouse/keyboard';
    else hint.textContent = 'Control active for ' + (controlledAgentId || 'agent');
  }
  if (bar) bar.classList.toggle('control-active', !!remoteControlEnabled);
  if (takeBtn) {
    if (remoteControlEnabled) {
      takeBtn.textContent = '✓ Controlling';
      takeBtn.classList.add('control-on');
      takeBtn.classList.remove('btn-primary');
    } else {
      takeBtn.textContent = 'Take Control';
      takeBtn.classList.remove('control-on');
      takeBtn.classList.add('btn-primary');
    }
  }
  if (releaseBtn) {
    if (remoteControlEnabled) {
      releaseBtn.classList.add('btn-primary');
      releaseBtn.classList.remove('btn-outline');
    } else {
      releaseBtn.classList.remove('btn-primary');
      releaseBtn.classList.add('btn-outline');
    }
  }
}

function updateCellHighlights() {
  Object.keys(agentCells).forEach(id => {
    const c = agentCells[id];
    if (!c || !c.cell) return;
    c.cell.classList.toggle('controlling', id === controlledAgentId && remoteControlEnabled);
    if (c.badgeEl) {
      c.badgeEl.textContent = id === controlledAgentId ? '🎮 Control' : 'Live';
      c.badgeEl.className = 'cell-badge ' + (id === controlledAgentId ? 'control' : 'live');
    }
  });
}

function ensureAgentCell(agentId) {
  if (agentCells[agentId]) return agentCells[agentId];
  const grid = document.getElementById('cctv-grid');
  const empty = document.getElementById('grid-empty');
  if (empty) empty.style.display = 'none';

  const cell = document.createElement('div');
  cell.className = 'cctv-cell';
  cell.dataset.agentId = agentId;
  cell.innerHTML =
    '<div class="cell-header">' +
      '<span>' +
        '<span class="cell-name" title="' + escapeHtml(agentId) + '">' + escapeHtml(agentId) + '</span>' +
        '<span class="cell-lanwan" title="LAN / WAN">—</span>' +
      '</span>' +
      '<span class="cell-mode" style="font-size:9px;padding:1px 5px;border-radius:3px;background:rgba(255,255,255,0.08);color:var(--text3);margin-right:4px" title="Mode">—</span>' +
      '<span class="cell-badge live">Live</span>' +
      '<span class="cell-actions" style="display:flex;gap:3px;margin-left:auto">' +
        '<button class="btn-promote auth-gated" onclick="event.stopPropagation();promoteAgent(\''+escapeHtml(agentId)+'\')" title="Promote to server" style="background:none;border:none;cursor:pointer;font-size:12px;padding:2px 6px;border-radius:4px;transition:background 0.2s" onmouseover="this.style.background=\'rgba(255,215,0,0.3)\'" onmouseout="this.style.background=\'none\'">👑</button>' +
        '<button class="btn-hide auth-gated" onclick="event.stopPropagation();toggleHideAgent(\''+escapeHtml(agentId)+'\')" title="Hide agent" style="background:none;border:none;cursor:pointer;font-size:12px;padding:2px 6px">👁</button>' +
        '<button class="btn-remove auth-gated" onclick="event.stopPropagation();removeAgent(\''+escapeHtml(agentId)+'\')" title="Remove agent" style="background:none;border:none;cursor:pointer;font-size:12px;padding:2px 6px">✕</button>' +
      '</span>' +
    '</div>' +
    '<div class="cell-body">' +
      '<div class="cell-empty">Waiting for first frame…</div>' +
      '<canvas class="cell-canvas"></canvas>' +
    '</div>' +
    '<div class="cell-info">' +
      '<span class="info-item"><span class="info-icon">🖥</span><span class="info-val info-hostname" title="Hostname">—</span></span>' +
      '<span class="info-item"><span class="info-icon">⏰</span><span class="info-val info-boot" title="System Boot Time">—</span></span>' +
      '<span class="info-item"><span class="info-icon">💤</span><span class="info-val info-idle" title="Total Idle Time">—</span></span>' +
      '<span class="info-item"><span class="info-icon">📡</span><span class="info-val info-transport" title="Transport">—</span></span>' +
      '<span class="info-item"><span class="info-icon">📶</span><span class="info-val info-health" title="Health">—</span></span>' +
      '<span class="info-item"><span class="info-icon">⚡</span><span class="info-val info-latency" title="Latency">—</span></span>' +
      '<span class="info-item"><span class="info-icon">📊</span><span class="info-val info-bandwidth" title="Bandwidth">—</span></span>' +
    '</div>';

  const canvas = cell.querySelector('.cell-canvas');
  const emptyEl = cell.querySelector('.cell-empty');
  const badgeEl = cell.querySelector('.cell-badge');
  const nameEl = cell.querySelector('.cell-name');
  const lanWanEl = cell.querySelector('.cell-lanwan');
  const hostnameEl = cell.querySelector('.info-hostname');
  const bootEl = cell.querySelector('.info-boot');
  const idleEl = cell.querySelector('.info-idle');
  const transportEl = cell.querySelector('.info-transport');
  const healthEl = cell.querySelector('.info-health');
  const latencyEl = cell.querySelector('.info-latency');
  const bandwidthEl = cell.querySelector('.info-bandwidth');
  const cellModeEl = cell.querySelector('.cell-mode');

  cell.addEventListener('click', () => {
    setControlledAgent(agentId);
  });
  cell.addEventListener('dblclick', (e) => {
    e.preventDefault();
    setControlledAgent(agentId);
    setViewMode('single');
    const rec = agentCells[agentId];
    if (rec) {
      const payload = rec.lastPayload || (rec.lastB64 ? { data: rec.lastB64 } : null);
      if (payload) renderToMainCanvas(agentId, payload);
    }
  });

  bindCanvasControl(canvas, agentId);
  grid.appendChild(cell);

  agentCells[agentId] = {
    cell, canvas, emptyEl, badgeEl, nameEl, lanWanEl, hostnameEl, bootEl, idleEl, cellModeEl, transportEl, healthEl, latencyEl, bandwidthEl,
    lastB64: null, lastTs: 0, imgW: 0, imgH: 0
  };
  knownAgentIds.add(agentId);
  if (authenticated) cell.querySelectorAll('.auth-gated').forEach(el => el.classList.add('visible'));
  if (!controlledAgentId) setControlledAgent(agentId, false);
  fetchAgentInfo(agentId);
  updateCellVisibility();
  return agentCells[agentId];
}

function escapeHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/"/g,'&quot;');
}

function syncAgentListFromKnown() {
  const list = [...knownAgentIds];
  const el = document.getElementById('agents-list');
  if (!el || !list.length) return;
  const html = list.map(id => {
    const isHidden = hiddenAgents.has(id);
    return '<div style="padding:4px 0;display:flex;justify-content:space-between;align-items:center">'+
      '<span style="cursor:pointer;color:var(--primary);flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" onclick="selectAgent(\''+escapeHtml(id).replace(/'/g,"\\'")+'\')">'+escapeHtml(id)+'</span>'+
      '<button onclick="event.stopPropagation();toggleHideAgent(\''+escapeHtml(id).replace(/'/g,"\\'")+'\')" title="'+(isHidden?'Show':'Hide')+'" style="background:none;border:none;cursor:pointer;font-size:12px;padding:2px 6px;margin-right:4px">'+(isHidden?'👁‍🗨':'👁')+'</button>'+
      '<button class="btn btn-sm btn-outline" style="font-size:8px;padding:2px 6px" onclick="focusAgent(\''+escapeHtml(id).replace(/'/g,"\\'")+'\')">Focus</button></div>';
  }).join('');
  if (el._lastHtml !== html) {
    el._lastHtml = html;
    el.innerHTML = html;
  }
}

function focusAgent(id) {
  setControlledAgent(id);
  setViewMode('single');
  agentsPanelOpen = false;
  document.getElementById('agents-panel').classList.remove('open');
  // Defer render and retry — the #screen-container only becomes visible/
  // dimensioned AFTER the CSS class flip, and a single rAF can land before
  // the browser has performed layout, returning clientWidth=0 and causing
  // doRenderToMainCanvas to bail out. Retry every 50ms for up to 500ms.
  const tryRender = (attempt) => {
    const rec = agentCells[id];
    const container = document.getElementById('screen-container');
    if (rec && (rec.lastB64 || rec.lastPayload) && container && container.clientWidth > 0 && container.clientHeight > 0) {
      const payload = rec.lastPayload || { data: rec.lastB64 };
      renderToMainCanvas(id, payload);
      return;
    }
    if (attempt < 10) {
      setTimeout(() => tryRender(attempt + 1), 50);
    } else {
      // Fallback: show placeholder so user knows we're waiting for a frame
      const placeholder = document.getElementById('screen-placeholder');
      if (placeholder) {
        placeholder.style.display = 'flex';
        placeholder.querySelector('div:nth-child(2)').textContent = 'Waiting for first frame from ' + id + '...';
      }
    }
  };
  setTimeout(() => tryRender(0), 50);
}

function drawToCanvas(canvas, img, maxW, maxH) {
  let w = img.width, h = img.height;
  const scale = Math.min(maxW / w, maxH / h, 2);
  w = Math.max(1, Math.round(w * scale));
  h = Math.max(1, Math.round(h * scale));
  // Only resize when dimensions actually change — avoids clearing the canvas unnecessarily
  if (canvas._drawW !== w || canvas._drawH !== h) {
    canvas.width = w;
    canvas.height = h;
    canvas._drawW = w;
    canvas._drawH = h;
  }
  const ctx = canvas.getContext('2d');
  ctx.imageSmoothingEnabled = true;
  ctx.imageSmoothingQuality = 'high';
  ctx.drawImage(img, 0, 0, w, h);
  return { w, h };
}

let _renderPending = {};
let _renderScheduled = {};
function scheduleFrame(agentId, payload) {
  _renderPending[agentId] = payload;
  if (_renderScheduled[agentId]) return;
  _renderScheduled[agentId] = true;
  requestAnimationFrame(() => {
    _renderScheduled[agentId] = false;
    const data = _renderPending[agentId] || payload;
    _renderPending[agentId] = null;
    doRenderToCell(agentId, data);
  });
}

let _renderVersion = {};
function doRenderToCell(agentId, payload) {
  const rec = agentCells[agentId];
  if (!rec) return;
  if (rec.cell.style.display === 'none' || rec.cell.classList.contains('agent-hidden')) return;
  // Version counter prevents stale Image.onload from overwriting newer frames
  const version = (_renderVersion[agentId] || 0) + 1;
  _renderVersion[agentId] = version;
  // If payload has a bitmap (from createImageBitmap), draw it directly —
  // already decoded, no Image.onload needed.
  if (payload && payload.bitmap) {
    const bmp = payload.bitmap;
    rec.emptyEl.style.display = 'none';
    rec.canvas.style.display = 'block';
    const body = rec.cell.querySelector('.cell-body');
    const cw = rec._bodyW || body.clientWidth;
    const ch = rec._bodyH || body.clientHeight;
    if (body.clientWidth !== rec._bodyW || body.clientHeight !== rec._bodyH) {
      rec._bodyW = body.clientWidth;
      rec._bodyH = body.clientHeight;
    }
    const dims = drawToCanvas(rec.canvas, bmp, cw, ch);
    rec.imgW = dims.w;
    rec.imgH = dims.h;
    rec.lastTs = Date.now();
    if (rec.badgeEl) {
      if (agentId === controlledAgentId && remoteControlEnabled && authenticated) {
        rec.badgeEl.textContent = '🎮 Control';
        rec.badgeEl.className = 'cell-badge control';
      } else {
        const isSrv = rec.cellModeEl && rec.cellModeEl.textContent === '★ SERVER';
        rec.badgeEl.textContent = isSrv ? '★ SERVER' : 'Live';
        rec.badgeEl.className = isSrv ? 'cell-badge server' : 'cell-badge live';
      }
    }
    // Free GPU memory ASAP
    if (bmp.close) bmp.close();
    return;
  }
  // Fallback: base64 data path for old browsers or when createImageBitmap fails
  const b64data = payload && payload.data;
  if (!b64data) return;
  const img = new Image();
  img.onload = () => {
    if (_renderVersion[agentId] !== version) return;
    rec.emptyEl.style.display = 'none';
    rec.canvas.style.display = 'block';
    const body = rec.cell.querySelector('.cell-body');
    const cw = body.clientWidth || 320;
    const ch = body.clientHeight || 240;
    const dims = drawToCanvas(rec.canvas, img, cw, ch);
    rec.imgW = dims.w;
    rec.imgH = dims.h;
    rec.lastTs = Date.now();
    if (rec.badgeEl) {
      if (agentId === controlledAgentId && remoteControlEnabled && authenticated) {
        rec.badgeEl.textContent = '🎮 Control';
        rec.badgeEl.className = 'cell-badge control';
      } else {
        rec.badgeEl.textContent = 'Live';
        rec.badgeEl.className = 'cell-badge live';
      }
    }
  };
  img.onerror = () => {
    if (_renderVersion[agentId] !== version) return;
    rec.emptyEl.querySelector('div:nth-child(2)').textContent = 'Failed to decode frame';
  };
  img.src = 'data:image/jpeg;base64,' + b64data;
}

function renderToCell(agentId, b64data) {
  scheduleFrame(agentId, b64data);
}

function renderToMainCanvas(agentId, b64data) {
  doRenderToMainCanvas(agentId, b64data);
}

let _mainCanvasScheduled = false;
let _pendingMain = null;
function scheduleMainFrame(agentId, payload) {
  _pendingMain = { agentId, payload };
  if (_mainCanvasScheduled) return;
  _mainCanvasScheduled = true;
  requestAnimationFrame(() => {
    _mainCanvasScheduled = false;
    const p = _pendingMain;
    _pendingMain = null;
    if (!p) return;
    doRenderToMainCanvas(p.agentId, p.payload);
  });
}

let _mainRenderVersion = 0;
function doRenderToMainCanvas(agentId, payload) {
  // Main canvas ALWAYS uses the base64 path (like v10.0.39 which worked).
  // The bitmap fast path is unreliable for the main canvas because:
  //   1. The same bitmap object is stored in rec.lastPayload — closing it
  //      would invalidate it for subsequent focusAgent clicks.
  //   2. createImageBitmap is async (Promise), which adds a render delay
  //      that can cause race conditions with the rAF scheduling.
  // The grid cells still use the bitmap fast path (many small renders benefit
  // most from the off-thread JPEG decode), but the main canvas is one big
  // fullscreen render where reliability matters more than 10ms speedup.
  const canvas = document.getElementById('screen-canvas');
  const placeholder = document.getElementById('screen-placeholder');
  const container = document.getElementById('screen-container');
  if (!container || container.clientWidth === 0 || container.clientHeight === 0) {
    return;
  }
  const version = ++_mainRenderVersion;
  // Accept both payload object and bare base64 string for backwards compat
  const b64data = (typeof payload === 'string') ? payload : (payload && payload.data);
  if (!b64data) return;
  const img = new Image();
  img.onload = () => {
    if (_mainRenderVersion !== version) return;
    placeholder.style.display = 'none';
    canvas.style.display = 'block';
    const dims = drawToCanvas(canvas, img, container.clientWidth - 28, container.clientHeight - 60);
    if (agentCells[agentId]) {
      agentCells[agentId].imgW = dims.w;
      agentCells[agentId].imgH = dims.h;
    }
  };
  img.onerror = () => {
    if (_mainRenderVersion !== version) return;
    placeholder.querySelector('div:nth-child(2)').textContent = 'Failed to decode frame';
  };
  img.src = 'data:image/jpeg;base64,' + b64data;
}

// ── Frame Rendering ──
let frameCount = 0;
let frameTimer = null;
function resetFrameTimer() {
  if (frameTimer) clearTimeout(frameTimer);
  frameTimer = setTimeout(() => {
    document.getElementById('status-dot').className = 'warning';
  }, 10000);
}
function renderFrame(agentId, payload) {
  frameCount++;
  resetFrameTimer();
  ensureAgentCell(agentId);
  const rec = agentCells[agentId];
  if (!rec) return;
  // Keep the base64 data around for resize re-renders (bitmap gets
  // .close()'d after draw, can't reuse). Also store payload for bitmap
  // fast path.
  if (payload.data) rec.lastB64 = payload.data;
  rec.lastPayload = payload;
  renderToCell(agentId, payload);
  if (viewMode === 'single' && controlledAgentId === agentId) {
    scheduleMainFrame(agentId, payload);
  }
  if (!controlledAgentId) setControlledAgent(agentId, false);
}

// ── Metrics ──
function updateMetrics(d) {
  if (d.agentId) activeAgentId = d.agentId;
  if (d.current_fps !== undefined) document.getElementById('m-fps').textContent = d.current_fps.toFixed(1);
  if (d.current_bandwidth_kbps !== undefined) document.getElementById('m-bw').textContent = Math.round(d.current_bandwidth_kbps);
  if (d.bytes_sent_mb !== undefined) document.getElementById('m-sent').textContent = d.bytes_sent_mb.toFixed(1);
  if (d.frames_dropped !== undefined) document.getElementById('m-drop').textContent = d.frames_dropped;
  if (d.current_quality !== undefined) document.getElementById('m-qual').textContent = d.current_quality;
  const secs = Math.floor((Date.now()-startTime)/1000);
  document.getElementById('m-uptime').textContent = secs+'s';
  if (d.monthly_limit_mb !== undefined) {
    document.getElementById('bw-slider').value = d.monthly_limit_mb;
    document.getElementById('bw-label').textContent = Math.round(d.monthly_limit_mb);
  }
  if (d.type === 'status') {
    if (d.mode) {
      serverMode = d.mode === 'server' || d.mode === 'standalone' || d.mode === 'server+agent';
      document.getElementById('mode-badge').textContent = d.mode === 'server+agent' ? 'SERVER+AGENT' : (serverMode ? 'SERVER' : 'AGENT');
    }
  }
  // Update the on-screen quality overlay (shown in single/focus mode)
  updateQualityOverlay(d);
}

// Update the FPS/latency/loss overlay in the focused view
function updateQualityOverlay(d) {
  const fpsEl = document.getElementById('qo-fps');
  const latEl = document.getElementById('qo-lat');
  const lossEl = document.getElementById('qo-loss');
  const dropEl = document.getElementById('qo-drop');
  const bwEl = document.getElementById('qo-bw');
  const tpEl = document.getElementById('qo-transport');
  if (!fpsEl) return;
  if (d.current_fps !== undefined) {
    fpsEl.textContent = d.current_fps.toFixed(1);
    fpsEl.dataset.warn = d.current_fps < 5 ? '2' : (d.current_fps < 10 ? '1' : '0');
  }
  if (d.latency_ms !== undefined) {
    latEl.textContent = d.latency_ms > 0 ? Math.round(d.latency_ms) : '—';
  } else if (d.avg_latency_ms !== undefined) {
    latEl.textContent = d.avg_latency_ms > 0 ? Math.round(d.avg_latency_ms) : '—';
  }
  if (d.packet_loss_pct !== undefined) {
    lossEl.textContent = d.packet_loss_pct.toFixed(1);
    lossEl.dataset.warn = d.packet_loss_pct > 5 ? '2' : (d.packet_loss_pct > 1 ? '1' : '0');
  } else if (d.packet_loss !== undefined) {
    lossEl.textContent = d.packet_loss.toFixed(1);
    lossEl.dataset.warn = d.packet_loss > 5 ? '2' : (d.packet_loss > 1 ? '1' : '0');
  }
  if (d.frames_dropped !== undefined) dropEl.textContent = d.frames_dropped;
  if (d.current_bandwidth_kbps !== undefined) bwEl.textContent = Math.round(d.current_bandwidth_kbps);
  if (d.transport !== undefined) tpEl.textContent = d.transport;
  if (d.type === 'status' && d.mode) tpEl.textContent = d.mode;
}

// ── Agents ──
let myAgentId = null; // this machine's own AgentID (stable across reboots)

function refreshAgents() {
   fetch('/api/system-info').then(r=>r.json()).then(info => {
     serverMode = info.mode === 'server' || info.mode === 'standalone' || info.mode === 'server+agent';
     document.getElementById('mode-badge').textContent = info.mode === 'server+agent' ? 'SERVER+AGENT' : (serverMode ? 'SERVER' : 'AGENT');
     if (info.agent_id) {
       const changed = myAgentId !== info.agent_id;
       myAgentId = info.agent_id;
       const fb = document.getElementById('focus-self-btn');
       if (fb) {
         fb.style.display = '';
         fb.title = 'Focus this machine (' + myAgentId + ')';
         if (changed) { /* ID updated */ }
       }
       // Show the stable machine ID in the topbar (only when not in server-mode
       // agent list to avoid duplication)
       const idEl = document.getElementById('my-agent-id');
       if (idEl) idEl.textContent = myAgentId;
     }
   }).catch(()=>{});
   // Single call to /api/agents/full – gets IDs + hidden status in one shot
   fetch('/api/agents/full').then(r=>r.json()).then(list => {
     if (!Array.isArray(list)) return;
     // Update hidden status map from full response
     const agentIds = [];
     list.forEach(a => {
       agentIds.push(a.id);
       if (a.hidden) hiddenAgents.add(a.id);
       else hiddenAgents.delete(a.id);
     });
     const el = document.getElementById('agents-list');
     if (agentIds.length === 0) {
       for (const id of Object.keys(agentCells)) {
         // Skip assist sessions — they manage their own lifecycle (manual close only)
         if (id.startsWith('assist-')) continue;
         agentCells[id].cell.remove();
         delete agentCells[id];
       }
       knownAgentIds.clear();
       if (el) el.innerHTML = '<div style="padding:4px 0;color:var(--text3)">No agents connected</div>';
       syncAgentListFromKnown();
       renderAgentSelect([]);
       return;
     }
      // Mark stale cells as disconnected instead of removing them (avoids grid reflow)
      const activeSet = new Set(agentIds);
      for (const id of Object.keys(agentCells)) {
        // Skip assist sessions — they manage their own lifecycle
        if (id.startsWith('assist-')) continue;
        if (!activeSet.has(id)) {
          // Keep the cell but show disconnected state (last frame dimmed, overlay on top)
          const rec = agentCells[id];
          rec.cell.style.opacity = '0.35';
          rec.cell.style.pointerEvents = 'none';
          if (rec.badgeEl) {
            rec.badgeEl.textContent = 'Offline';
            rec.badgeEl.className = 'cell-badge';
            rec.badgeEl.style.background = 'rgba(255,0,0,.2)';
            rec.badgeEl.style.color = '#ef5350';
          }
          if (rec.emptyEl) {
            rec.emptyEl.textContent = 'Agent offline';
            rec.emptyEl.style.display = 'flex';
            rec.emptyEl.style.background = 'rgba(0,0,0,0.3)';
            rec.emptyEl.style.color = '#ef5350';
          }
          // Keep canvas visible so last frame remains
         knownAgentIds.delete(id);
         if (controlledAgentId === id) controlledAgentId = null;
         // Delay full removal to avoid rapid reconnect churn
         if (!rec._disconnectTimer) {
           rec._disconnectTimer = setTimeout(() => {
             if (rec.cell && rec.cell.parentNode) rec.cell.remove();
             delete agentCells[id];
           }, 30000);
         }
        } else {
          // Restore cell if it was marked disconnected
          const rec = agentCells[id];
          if (rec._disconnectTimer) {
            clearTimeout(rec._disconnectTimer);
            rec._disconnectTimer = null;
          }
          rec.cell.style.opacity = '';
          rec.cell.style.pointerEvents = '';
          rec.emptyEl.style.display = 'none';
          rec.emptyEl.style.background = 'transparent';
          rec.emptyEl.style.color = '';
          if (rec.badgeEl) {
            rec.badgeEl.textContent = id === controlledAgentId ? '🎮 Control' : 'Live';
            rec.badgeEl.className = 'cell-badge ' + (id === controlledAgentId ? 'control' : 'live');
            rec.badgeEl.style.background = '';
            rec.badgeEl.style.color = '';
          }
        }
     }
      agentIds.forEach(id => {
        knownAgentIds.add(id);
        if (!agentCells[id]) ensureAgentCell(id);
        fetchAgentInfo(id);
      });
      // Mark server agent cells with a ★ badge
      list.forEach(a => {
        const rec = agentCells[a.id];
        if (rec && rec.cellModeEl) {
          rec.cellModeEl.textContent = a.is_server ? '★ SERVER' : '—';
          rec.cellModeEl.style.background = a.is_server ? 'rgba(255,215,0,0.2)' : 'rgba(255,255,255,0.08)';
          rec.cellModeEl.style.color = a.is_server ? '#ffd700' : 'var(--text3)';
        }
      });
      syncAgentListFromKnown();
      renderAgentSelect(agentIds);
      // Apply hidden class in one pass
      for (const [agentId, cellObj] of Object.entries(agentCells)) {
        cellObj.cell.classList.toggle('agent-hidden', hiddenAgents.has(agentId));
      }
    }).catch(()=>{});
    refreshAgentStats();
}

function refreshAgentStats() {
  fetch('/api/agent-stats').then(r=>r.json()).then(stats => {
    for (const [agentId, s] of Object.entries(stats)) {
      const rec = agentCells[agentId];
      if (!rec) continue;
      if (rec.transportEl) {
        const t = s.transport || '—';
        const colors = { webrtc: '#4caf50', websocket: '#2196f3', github: '#ff9800', quic: '#9c27b0', self: '#ce93d8' };
        rec.transportEl.innerHTML = '<span style="display:inline-block;width:6px;height:6px;border-radius:50%;background:' + (colors[t]||'#888') + ';margin-right:3px"></span>' + t;
      }
      if (rec.healthEl) {
        const h = s.health || '—';
        const hcolors = { excellent: '#4caf50', good: '#4caf50', fair: '#ff9800', poor: '#ef5350', slow: '#ff9800', stale: '#ef5350', waiting: '#888', unknown: '#888' };
        rec.healthEl.innerHTML = '<span style="display:inline-block;width:6px;height:6px;border-radius:50%;background:' + (hcolors[h]||'#888') + ';margin-right:3px"></span>' + h;
      }
      if (rec.latencyEl) {
        // "self" transport shows capture-to-broadcast time; <8ms is typical
        // for local JPEG encode + in-process send.
        if (s.transport === 'self') {
          rec.latencyEl.textContent = s.latency_ms > 0 ? Math.round(s.latency_ms) + 'ms (local)' : '—';
        } else {
          rec.latencyEl.textContent = s.latency_ms > 0 ? Math.round(s.latency_ms) + 'ms' : '—';
        }
      }
      if (rec.bandwidthEl) {
        const kbps = s.bytes_per_sec || 0;
        if (kbps > 1024) {
          rec.bandwidthEl.textContent = (kbps/1024).toFixed(1) + ' MB/s';
        } else {
          rec.bandwidthEl.textContent = Math.round(kbps) + ' KB/s';
        }
      }
    }
  }).catch(()=>{});
}

function refreshServerLoad() {
  fetch('/api/server-load').then(r=>r.json()).then(d => {
    if (d.cpu_percent !== undefined) document.getElementById('sl-cpu').textContent = d.cpu_percent + '%';
    if (d.mem_percent !== undefined) document.getElementById('sl-mem').textContent = d.mem_percent + '%';
    if (d.ws_connections !== undefined) document.getElementById('sl-ws').textContent = d.ws_connections;
    if (d.agent_count !== undefined) document.getElementById('sl-agents').textContent = d.agent_count;
    if (d.net_recv_rate !== undefined) document.getElementById('sl-net-in').textContent = Math.round(d.net_recv_rate);
    if (d.net_sent_rate !== undefined) document.getElementById('sl-net-out').textContent = Math.round(d.net_sent_rate);
  }).catch(()=>{});
}

function refreshElectionStatus() {
  fetch('/api/election-status').then(r=>r.json()).then(d => {
    const el = document.getElementById('election-badge');
    if (!el) return;
    let icon = '⚙';
    let label = '';
    let color = 'rgba(255,255,255,.18)';
    // Show GitHub auth failure prominently
    if (d.configured && d.github_auth_ok === false) {
      icon = '🔑';
      label = 'GH AUTH FAIL';
      color = 'rgba(244,67,54,.7)';
      el.textContent = icon + ' ' + label;
      el.style.background = color;
      el.title = 'GitHub authentication failed: ' + (d.github_auth_err || 'unknown') + '\nClick → live events | Update token in Settings';
      showGitHubAuthBanner(d.github_auth_err || 'Authentication failed', d.token_masked);
      return;
    } else {
      hideGitHubAuthBanner();
    }
    if (d.method === 'github') {
      if (d.self_is_leader) {
        icon = '👑';
        label = 'GITHUB LEADER';
        color = 'rgba(76,175,80,.5)';
      } else if (d.last_result === 'active' && d.leader_id) {
        icon = '🛰';
        label = 'GITHUB · ' + (d.leader_id.length > 14 ? d.leader_id.slice(0, 12) + '…' : d.leader_id);
        color = 'rgba(33,150,243,.4)';
      } else if (d.last_result === 'claimed' || d.last_result === 'renewed') {
        icon = '✅';
        label = 'GITHUB ' + d.last_result.toUpperCase();
        color = 'rgba(76,175,80,.5)';
      } else if (d.last_error) {
        icon = '⚠';
        label = 'GITHUB ERR';
        color = 'rgba(244,67,54,.5)';
      } else {
        icon = '🛰';
        label = 'GITHUB';
        color = 'rgba(33,150,243,.4)';
      }
    } else if (d.method === 'relay') {
      icon = '🔁';
      label = 'RELAY AGENT';
      color = 'rgba(255,152,0,.4)';
    } else if (d.method === 'lan') {
      icon = '📡';
      label = 'LAN';
      color = 'rgba(156,204,101,.4)';
    } else if (!d.configured) {
      icon = '⚙';
      label = 'NO GITHUB';
      color = 'rgba(158,158,158,.4)';
    } else {
      icon = '⚙';
      label = d.last_result ? d.last_result.toUpperCase() : 'ELECTION';
      color = 'rgba(255,255,255,.18)';
    }
    el.textContent = icon + ' ' + label;
    el.style.background = color;
    el.title = d.configured
      ? ('GitHub configured: ' + (d.repo || '(none)') + ' | Token: ' + (d.token_masked || '(none)') + ' | Method: ' + d.method + ' | Leader: ' + (d.leader_id || '(none)') + ' | Result: ' + (d.last_result || '(none)') + (d.last_error ? ' | Error: ' + d.last_error : '') + '\nClick → live election events')
      : 'GitHub NOT configured — using relay/LAN fallback\nClick → live election events';
  }).catch(()=>{});
}

let githubAuthBannerDismissed = false;
function showGitHubAuthBanner(errStr, tokenMasked) {
  if (githubAuthBannerDismissed) return;
  const banner = document.getElementById('github-auth-banner');
  if (!banner) return;
  document.getElementById('github-auth-err').textContent = errStr + (tokenMasked ? '   (token: ' + tokenMasked + ')' : '');
  banner.style.display = 'flex';
}
function hideGitHubAuthBanner() {
  const banner = document.getElementById('github-auth-banner');
  if (banner) banner.style.display = 'none';
}
function dismissGitHubAuthBanner() {
  githubAuthBannerDismissed = true;
  hideGitHubAuthBanner();
}

function openGitHubReauthModal() {
  document.getElementById('github-reauth-err').style.display = 'none';
  document.getElementById('github-reauth-ok').style.display = 'none';
  // Pre-fill the current repo
  fetch('/api/settings').then(r=>r.json()).then(s => {
    if (s.github_repo) document.getElementById('reauth-repo').value = s.github_repo;
  }).catch(()=>{});
  document.getElementById('github-reauth-modal').style.display = 'flex';
  setTimeout(() => document.getElementById('reauth-token').focus(), 100);
}
function closeGitHubReauthModal() {
  document.getElementById('github-reauth-modal').style.display = 'none';
}
async function testGitHubTokenFromModal() {
  const token = document.getElementById('reauth-token').value.trim();
  const repo = document.getElementById('reauth-repo').value.trim();
  const errBox = document.getElementById('github-reauth-err');
  const okBox = document.getElementById('github-reauth-ok');
  errBox.style.display = 'none';
  okBox.style.display = 'none';
  if (!token) {
    errBox.textContent = 'Enter a token first. (Click the link above to generate one at GitHub.)';
    errBox.style.display = 'block';
    document.getElementById('reauth-token').focus();
    return;
  }
  // Basic format check
  if (token.length < 20) {
    errBox.textContent = 'That token is too short (' + token.length + ' chars). A valid GitHub token is 40+ characters. Did you paste the full token?';
    errBox.style.display = 'block';
    return;
  }
  errBox.textContent = 'Testing… (calling GitHub /user endpoint)';
  errBox.style.color = '#aaa';
  errBox.style.display = 'block';
  try {
    const resp = await fetch('/api/github/auth-test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token, repo })
    });
    const data = await resp.json();
    errBox.style.color = '#fcc';
    if (data.ok || data.token_ok) {
      errBox.style.display = 'none';
      let msg = '✓ Token works';
      if (data.user) msg += ' — authenticated as: ' + data.user;
      if (data.hint) msg += '\n' + data.hint;
      okBox.textContent = msg;
      okBox.style.display = 'block';
    } else {
      errBox.textContent = '✗ Token rejected by GitHub:\n\n' + (data.error || 'Unknown error') + '\n\nFollow the step-by-step guide above to generate a working token.';
      errBox.style.display = 'block';
    }
  } catch (e) {
    errBox.textContent = 'Network error: ' + e + '\n(Is the PunMonitor service running?)';
    errBox.style.display = 'block';
  }
}
async function saveGitHubTokenFromModal() {
  const token = document.getElementById('reauth-token').value.trim();
  const repo = document.getElementById('reauth-repo').value.trim();
  const errBox = document.getElementById('github-reauth-err');
  const okBox = document.getElementById('github-reauth-ok');
  errBox.style.display = 'none';
  okBox.style.display = 'none';
  if (!token) {
    errBox.textContent = 'Enter a token first.';
    errBox.style.display = 'block';
    document.getElementById('reauth-token').focus();
    return;
  }
  if (token.length < 20) {
    errBox.textContent = 'That token is too short (' + token.length + ' chars). Paste the full token from GitHub.';
    errBox.style.display = 'block';
    return;
  }
  errBox.textContent = 'Saving token and re-testing against GitHub…';
  errBox.style.color = '#aaa';
  errBox.style.display = 'block';
  try {
    const resp = await fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ github_token: token, github_repo: repo || undefined })
    });
    const data = await resp.json();
    errBox.style.color = '#fcc';
    if (data.github_auth && data.github_auth.ok) {
      errBox.style.display = 'none';
      let msg = '✓ Saved and verified. Auth OK';
      if (data.github_auth.user) msg += ' as: ' + data.github_auth.user;
      msg += '.\n\nElection will resume on the next tick. Daily report will be re-pushed to GitHub at the next scheduled time.';
      okBox.textContent = msg;
      okBox.style.display = 'block';
      githubAuthBannerDismissed = true;
      hideGitHubAuthBanner();
      setTimeout(() => {
        closeGitHubReauthModal();
        refreshElectionStatus();
      }, 2500);
    } else {
      errBox.textContent = '✗ Token saved to disk but GitHub still rejects it:\n\n' + (data.github_auth ? data.github_auth.error : 'unknown') + '\n\nThis means the token is invalid. Generate a NEW token at https://github.com/settings/tokens (use the step-by-step guide above).';
      errBox.style.display = 'block';
    }
  } catch (e) {
    errBox.textContent = 'Network error: ' + e;
    errBox.style.display = 'block';
  }
}

// === Live Election Events Modal ===
function openElectionEventsModal() {
  document.getElementById('election-events-modal').style.display = 'flex';
  refreshElectionEventsModal();
}
function closeElectionEventsModal() {
  document.getElementById('election-events-modal').style.display = 'none';
}
async function refreshElectionEventsModal() {
  try {
    const resp = await fetch('/api/election-history');
    const data = await resp.json();
    const events = (data.events || []).slice().reverse(); // newest first
    document.getElementById('election-events-count').textContent = events.length + ' events';
    const statusDiv = document.getElementById('election-events-status');
    if (data.github_auth_ok === false) {
      statusDiv.innerHTML = '<span style="color:#fcc">⚠ GitHub auth FAILED:</span> ' + (data.github_auth_err || 'unknown') + ' &nbsp; <button onclick="openGitHubReauthModal()" style="background:#4a9eff;color:#fff;border:none;padding:3px 8px;border-radius:3px;font-size:10px;cursor:pointer">🔑 Update token</button>';
    } else if (data.github_auth_ok === true) {
      statusDiv.innerHTML = '<span style="color:#9f9">✓ GitHub auth OK</span> &nbsp; Last push: ' + (data.last_push || '(none)');
    } else {
      statusDiv.textContent = 'Last push: ' + (data.last_push || '(none)');
    }
    const tbody = document.getElementById('election-events-tbody');
    tbody.innerHTML = events.map(ev => {
      const t = new Date(ev.timestamp).toLocaleTimeString();
      const actionColor = ev.action === 'error' ? '#fcc' : (ev.action === 'renewed' ? '#9f9' : (ev.action === 'claimed' ? '#9ff' : (ev.action === 'stale-takeover' ? '#fc9' : '#ccc')));
      const agentShort = (ev.agent_id || '').length > 22 ? (ev.agent_id.slice(0, 20) + '…') : ev.agent_id;
      const leaderShort = (ev.leader_id || '').length > 22 ? (ev.leader_id.slice(0, 20) + '…') : (ev.leader_id || '—');
      const ageStr = ev.leader_age_ms > 0 && ev.leader_age_ms < 9223372036854 ? Math.round(ev.leader_age_ms/1000) + 's' : '—';
      return '<tr style="border-bottom:1px solid #2a2a2a">' +
        '<td style="padding:4px 6px;color:#888">' + t + '</td>' +
        '<td style="padding:4px 6px;color:' + actionColor + ';font-weight:600">' + ev.action + '</td>' +
        '<td style="padding:4px 6px;color:#aaa">' + ev.method + '</td>' +
        '<td style="padding:4px 6px;color:#7cf">' + agentShort + '</td>' +
        '<td style="padding:4px 6px;color:#cf9">' + leaderShort + '</td>' +
        '<td style="padding:4px 6px;text-align:right;color:#888">' + ageStr + '</td>' +
        '<td style="padding:4px 6px;color:' + actionColor + '">' + (ev.result || '—') + '</td>' +
        '<td style="padding:4px 6px;color:#f88;font-size:10px">' + (ev.error || '') + '</td>' +
      '</tr>';
    }).join('');
  } catch (e) {
    document.getElementById('election-events-tbody').innerHTML = '<tr><td colspan="8" style="padding:12px;color:#fcc">Error: ' + e + '</td></tr>';
  }
}
// Auto-refresh election events modal when open
setInterval(() => {
  const m = document.getElementById('election-events-modal');
  if (m && m.style.display === 'flex') refreshElectionEventsModal();
}, 3000);

// === Historical Reports Modal ===
function openHistoricalReportsModal() {
  document.getElementById('historical-reports-modal').style.display = 'flex';
  refreshHistoricalReportsList();
}
function closeHistoricalReportsModal() {
  document.getElementById('historical-reports-modal').style.display = 'none';
}
async function refreshHistoricalReportsList() {
  const listEl = document.getElementById('historical-reports-list');
  listEl.innerHTML = '<div style="padding:20px;text-align:center;color:#888">⏳ Loading from GitHub…</div>';
  try {
    const resp = await fetch('/api/reports/list');
    const data = await resp.json();
    if (!data.ok) {
      listEl.innerHTML = '<div style="padding:12px;background:rgba(255,80,80,.15);border-radius:4px;color:#fcc">✗ ' + (data.error || 'Unknown error') + '</div>';
      return;
    }
    if (!data.reports || data.reports.length === 0) {
      listEl.innerHTML = '<div style="padding:12px;color:#888">No report-*.xlsx files in the repo yet. They get auto-pushed daily at 00:05 local time.</div>';
      return;
    }
    listEl.innerHTML = data.reports.map(r => {
      const sizeKB = (r.size / 1024).toFixed(1);
      const dateStr = r.name.replace('report-', '').replace('.xlsx', '');
      return '<div style="display:flex;align-items:center;gap:8px;padding:8px;border-bottom:1px solid #2a2a2a">' +
        '<span style="color:#7cf;min-width:90px">📅 ' + dateStr + '</span>' +
        '<span style="color:#888;min-width:60px">' + sizeKB + ' KB</span>' +
        '<a href="' + r.raw_url + '" target="_blank" style="color:#4a9eff;text-decoration:none">⬇ Download</a>' +
        '<a href="' + r.html_url + '" target="_blank" style="color:#888;text-decoration:none">🔗 GitHub</a>' +
        '</div>';
    }).join('');
  } catch (e) {
    listEl.innerHTML = '<div style="padding:12px;color:#fcc">Error: ' + e + '</div>';
  }
}
function downloadMergedReport() {
  // /api/reports/merged streams an XLSX with all daily reports concatenated
  const a = document.createElement('a');
  a.href = '/api/reports/merged';
  a.download = 'punmonitor-merged-' + new Date().toISOString().slice(0, 10) + '.xlsx';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

// ---- Software Update flow ----
// Backend:
//   GET  /api/check-update    -> { available, current_version, latest_version, download_url, ... }
//   POST /api/update          -> { ok, msg, url } — downloads new binary, replaces itself, broadcasts to agents
//
// Behavior:
//   - One button in topbar opens a modal
//   - "Check now" calls /api/check-update and shows result
//   - If a newer version is available, "Update all machines" becomes enabled
//   - Clicking it: POST /api/update with the download URL, server updates itself
//     and broadcasts the URL to all connected agents, which download + replace
//   - Watchdog (3s delay) restarts everything; this dashboard tab will lose
//     connection and reload automatically once the new server is up
let _updateCheckInFlight = false;
let _updateApplyInFlight = false;
let _latestUpdateInfo = null;

async function checkForUpdates(openModal = true) {
  if (_updateCheckInFlight) return;
  _updateCheckInFlight = true;

  // Open modal on first call from the button
  if (openModal) openUpdateModal();

  const statusEl = document.getElementById('update-status');
  const infoEl = document.getElementById('update-info');
  const applyBtn = document.getElementById('update-apply-btn');
  const checkBtn = document.getElementById('update-check-btn');
  applyBtn.disabled = true;
  applyBtn.style.opacity = '.5';
  applyBtn.textContent = '⬇ Update all machines';
  _latestUpdateInfo = null;

  // Show "checking…" state
  statusEl.style.display = 'block';
  statusEl.style.background = 'rgba(80,160,255,.1)';
  statusEl.style.border = '1px solid rgba(80,160,255,.3)';
  statusEl.style.color = '#7cf';
  statusEl.textContent = '⏳ Checking GitHub for new releases…';
  infoEl.style.display = 'none';
  checkBtn.disabled = true;
  checkBtn.textContent = '⏳ Checking…';

  try {
    const resp = await fetch('/api/check-update', { cache: 'no-store' });
    const data = await resp.json();
    document.getElementById('update-current-version').textContent = data.current_version || '—';
    if (data.available && data.download_url) {
      _latestUpdateInfo = data;
      statusEl.style.background = 'rgba(80,200,80,.15)';
      statusEl.style.border = '1px solid rgba(80,200,80,.4)';
      statusEl.style.color = '#8f8';
      statusEl.textContent = '✅ New version available: ' + data.latest_version + ' (you have ' + data.current_version + ')';
      infoEl.style.display = 'block';
      document.getElementById('update-latest-version').textContent = data.latest_version;
      document.getElementById('update-published').textContent = data.published_at ? '— released ' + new Date(data.published_at).toLocaleString() : '';
      document.getElementById('update-size').textContent = data.size_mb ? data.size_mb.toFixed(2) + ' MB' : '—';
      const htmlUrl = document.getElementById('update-html-url');
      htmlUrl.href = data.html_url || '#';
      if (data.notes) {
        document.getElementById('update-notes-wrap').style.display = 'block';
        document.getElementById('update-notes').textContent = data.notes;
      } else {
        document.getElementById('update-notes-wrap').style.display = 'none';
      }
      applyBtn.disabled = false;
      applyBtn.style.opacity = '1';
      // Show the green dot on the topbar button so user knows there's something
      const dot = document.getElementById('update-dot');
      if (dot) dot.style.display = 'inline-block';
    } else {
      statusEl.style.background = 'rgba(120,120,120,.15)';
      statusEl.style.border = '1px solid #444';
      statusEl.style.color = '#aaa';
      statusEl.textContent = '✓ You are on the latest version (' + (data.current_version || '—') + '). ' + (data.reason || 'No newer release on GitHub.');
      infoEl.style.display = 'none';
      _latestUpdateInfo = null;
    }
  } catch (e) {
    statusEl.style.background = 'rgba(255,80,80,.15)';
    statusEl.style.border = '1px solid rgba(255,80,80,.4)';
    statusEl.style.color = '#fcc';
    statusEl.textContent = '✗ Error contacting update server: ' + e;
  } finally {
    _updateCheckInFlight = false;
    checkBtn.disabled = false;
    checkBtn.textContent = '🔍 Check now';
  }
}

async function applyUpdate() {
  if (_updateApplyInFlight) return;
  if (!_latestUpdateInfo || !_latestUpdateInfo.download_url) {
    alert('Click "Check now" first to find a newer version.');
    return;
  }
  const url = _latestUpdateInfo.download_url;
  const sizeMB = _latestUpdateInfo.size_mb || 0;
  const ok = confirm(
    'Update to v' + _latestUpdateInfo.latest_version + ' (' + sizeMB.toFixed(2) + ' MB)?\n\n' +
    'This will:\n' +
    '  1. Download the new binary\n' +
    '  2. Replace the running exe (this dashboard will disconnect for ~5 seconds)\n' +
    '  3. Watchdog restarts the new version\n' +
    '  4. All connected agents will be notified and update themselves\n\n' +
    'Continue?'
  );
  if (!ok) return;
  _updateApplyInFlight = true;

  const applyBtn = document.getElementById('update-apply-btn');
  const checkBtn = document.getElementById('update-check-btn');
  const progressEl = document.getElementById('update-apply-progress');
  const stageEl = document.getElementById('update-apply-stage');
  applyBtn.disabled = true;
  applyBtn.style.opacity = '.5';
  checkBtn.disabled = true;
  progressEl.style.display = 'block';
  stageEl.textContent = '⏬ Downloading from GitHub…';

  try {
    const resp = await fetch('/api/update', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: url })
    });
    const data = await resp.json();
    if (data.ok) {
      stageEl.textContent = '✓ Update started on this server. New version will be live in ~5 seconds.';
      // Wait a bit, then try to reload the page
      setTimeout(() => {
        stageEl.textContent = '⏳ Waiting for new version to come up…';
      }, 2000);
      setTimeout(() => {
        // Hard reload — bypasses HTTP cache
        window.location.reload(true);
      }, 6000);
    } else {
      stageEl.textContent = '✗ Update failed: ' + (data.error || data.msg || 'unknown error');
      _updateApplyInFlight = false;
      applyBtn.disabled = false;
      applyBtn.style.opacity = '1';
      checkBtn.disabled = false;
    }
  } catch (e) {
    stageEl.textContent = '✗ Network error: ' + e + ' — the server may have restarted anyway. Reload in a few seconds.';
    _updateApplyInFlight = false;
    applyBtn.disabled = false;
    applyBtn.style.opacity = '1';
    checkBtn.disabled = false;
  }
}

function openUpdateModal() {
  document.getElementById('update-modal').style.display = 'flex';
  // Show current version immediately
  if (window._serverVersion) {
    document.getElementById('update-current-version').textContent = window._serverVersion;
  }
  // Auto-check on first open
  if (!document.getElementById('update-info').style.display ||
      document.getElementById('update-info').style.display === 'none') {
    checkForUpdates(false);
  }
}
function closeUpdateModal() {
  document.getElementById('update-modal').style.display = 'none';
}

function updateCellVisibility() {
  const hasFilter = selectedAgents.size > 0;
  for (const [id, rec] of Object.entries(agentCells)) {
    const show = !hasFilter || selectedAgents.has(id);
    rec.cell.style.display = show ? '' : 'none';
  }
}

function toggleAgentSelection(id) {
  if (selectedAgents.has(id)) {
    selectedAgents.delete(id);
  } else {
    selectedAgents.add(id);
  }
  document.querySelectorAll('.agent-pill').forEach(p => {
    p.classList.toggle('selected', selectedAgents.has(p.textContent));
  });
  updateCellVisibility();
}

function selectAgent(id) {
  setControlledAgent(id);
  agentsPanelOpen = false;
  document.getElementById('agents-panel').classList.remove('open');
}

function renderAgentSelect(list) {
  let container = document.getElementById('agent-pills-container');
  if (!container) {
    container = document.createElement('div');
    container.id = 'agent-pills-container';
    container.className = 'agent-pills';
    document.getElementById('agent-selector-container').appendChild(container);
  }
  const key = list.join(',') + '|' + controlledAgentId + '|' + [...selectedAgents].sort().join(',');
  if (container._lastKey === key) return;
  container._lastKey = key;
  container.innerHTML = '';
  // Create dropdown toggle
  const toggle = document.createElement('button');
  toggle.className = 'agent-dropdown-toggle';
  const selectedCount = selectedAgents.size;
  toggle.textContent = selectedCount > 0 ? selectedCount + ' selected' : 'All agents';
  toggle.title = 'Click to select/deselect agents';
  container.appendChild(toggle);

  // Create dropdown list
  const dd = document.createElement('div');
  dd.className = 'agent-dropdown';
  list.forEach(id => {
    knownAgentIds.add(id);
    const item = document.createElement('label');
    item.className = 'agent-dropdown-item';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = selectedAgents.has(id) || selectedAgents.size === 0;
    cb.addEventListener('change', () => {
      toggleAgentSelection(id);
      toggle.textContent = selectedAgents.size > 0 ? selectedAgents.size + ' selected' : 'All agents';
    });
    const span = document.createElement('span');
    span.textContent = id;
    item.appendChild(cb);
    item.appendChild(span);
    dd.appendChild(item);
  });
  container.appendChild(dd);

  // Toggle dropdown visibility
  toggle.addEventListener('click', (e) => {
    e.stopPropagation();
    dd.classList.toggle('open');
  });
  document.addEventListener('click', () => dd.classList.remove('open'), { once: false });

  if (!controlledAgentId && list.length >= 1) {
    setControlledAgent(list[0], false);
  }
  updateFileTransferAgents();
}

// ── System Info ──
function fetchAgentInfo(agentId) {
   fetch('/api/agent-system-info/' + agentId).then(r => r.json()).then(info => {
     const rec = agentCells[agentId];
     if (!rec) return;
      if (info.hostname && rec.hostnameEl) rec.hostnameEl.textContent = info.hostname;
      if (info.mode && rec.cellModeEl) {
        const m = info.mode.toUpperCase();
        rec.cellModeEl.textContent = m;
        rec.cellModeEl.style.background = m === 'SERVER' ? 'rgba(76,175,80,0.25)' : m === 'AGENT' ? 'rgba(33,150,243,0.2)' : 'rgba(255,255,255,0.08)';
        rec.cellModeEl.style.color = m === 'SERVER' ? '#4caf50' : m === 'AGENT' ? '#2196f3' : 'var(--text3)';
      }
     if (rec.lanWanEl) {
       const local = info.local_ip || '—';
       const wan = info.wan_ip || '—';
       rec.lanWanEl.textContent = 'LAN: ' + local + ' / WAN: ' + wan;
     }
if (info.boot_time && rec.bootEl) rec.bootEl.textContent = info.boot_time;
      if (info.idle_time && rec.idleEl) rec.idleEl.textContent = info.idle_time;
   }).catch(() => {});
}

// ── Quality Controls ──
document.getElementById('bw-slider').addEventListener('input', function() {
  document.getElementById('bw-label').textContent = this.value;
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({type:'set_bandwidth_limit', mb:parseInt(this.value)}));
});
document.getElementById('tier-select').addEventListener('change', function() {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({type:'set_tier', tier:parseInt(this.value)}));
});
document.getElementById('qual-slider').addEventListener('input', function() {
  document.getElementById('qual-label').textContent = this.value;
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({type:'set_quality', quality:parseInt(this.value)}));
});
document.getElementById('fps-slider').addEventListener('input', function() {
  document.getElementById('fps-label').textContent = parseFloat(this.value).toFixed(1);
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({type:'set_fps', fps:parseFloat(this.value)}));
});

// ── Remote control (active by default in focus/single view) ──
function sendControl(payload, agentId) {
  if (!authenticated || !remoteControlEnabled || !ws || ws.readyState !== WebSocket.OPEN) return;
  const aid = agentId || controlledAgentId;
  if (!aid) return;
  payload.agentId = aid;
  ws.send(JSON.stringify(payload));
}

function bindCanvasControl(canvas, agentId) {
  canvas.addEventListener('mousedown', e => {
    if (agentId) {
      setControlledAgent(agentId);
      if (viewMode === 'grid') e.stopPropagation();
    }
    const aid = agentId || controlledAgentId;
    if (!aid || !remoteControlEnabled) return;
    const r = canvas.getBoundingClientRect();
    if (!r.width || !r.height) return;
    const sx = canvas.width / r.width;
    const sy = canvas.height / r.height;
    sendControl({
      type: 'mouse_click',
      button: e.button === 0 ? 'left' : e.button === 2 ? 'right' : 'middle',
      x: Math.round((e.clientX - r.left) * sx),
      y: Math.round((e.clientY - r.top) * sy)
    }, aid);
  });
  canvas.addEventListener('mousemove', e => {
    const aid = agentId || controlledAgentId;
    if (!remoteControlEnabled || !aid || controlledAgentId !== aid) return;
    const now = Date.now();
    const key = aid + ':mv';
    if (gridThrottle[key] && now - gridThrottle[key] < 16) return;
    gridThrottle[key] = now;
    const r = canvas.getBoundingClientRect();
    if (!r.width || !r.height) return;
    const sx = canvas.width / r.width;
    const sy = canvas.height / r.height;
    sendControl({
      type: 'mouse_move',
      x: Math.round((e.clientX - r.left) * sx),
      y: Math.round((e.clientY - r.top) * sy)
    }, aid);
  });
  canvas.addEventListener('contextmenu', e => e.preventDefault());
}

(function() {
  bindCanvasControl(document.getElementById('screen-canvas'));
  document.addEventListener('keydown', e => {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT' || e.target.tagName === 'TEXTAREA') return;
    if (!remoteControlEnabled || !controlledAgentId) return;
    sendControl({ type: 'key_press', key: e.keyCode });
    e.preventDefault();
  });
})();

// ── Gear / Login ──
function toggleGear() {
  location.href = '/admin';
}

function adminLogin() {
  const user = document.getElementById('admin-login-user').value.trim() || 'admin';
  const pass = document.getElementById('admin-login-pass').value.trim() || 'puneet12';
  const authUser = document.getElementById('admin-auth-user').value.trim() || 'puneet';
  const authPass = document.getElementById('admin-auth-pass').value.trim() || 'puneet12';
  if (user === authUser && pass === authPass) {
    settingsLoggedIn = true;
    authenticated = true;
    remoteControlEnabled = false;
    sessionStorage.setItem('punmonitor_auth', '1');
    document.getElementById('admin-login-section').style.display = 'none';
    document.getElementById('admin-settings-section').style.display = 'block';
    document.querySelectorAll('.auth-gated').forEach(el => el.classList.add('visible'));
    updateCellActions();
    updateControlBar();
    fetch('/api/settings').then(r=>r.json()).then(s => {
      if (s.github_repo) document.getElementById('admin-github-repo').value = s.github_repo;
      if (s.auth_user) document.getElementById('admin-auth-user').value = s.auth_user;
      if (s.auth_pass) document.getElementById('admin-auth-pass').value = s.auth_pass;
      if (s.server_url) document.getElementById('admin-server-url').value = s.server_url;
      if (s.tunnel_hostname) document.getElementById('admin-cf-hostname').value = s.tunnel_hostname;
      if (s.cloudflare_account_tag) document.getElementById('admin-cf-account-tag').value = s.cloudflare_account_tag;
      if (s.cloudflare_tunnel_secret) document.getElementById('admin-cf-tunnel-secret').value = s.cloudflare_tunnel_secret;
      if (s.cloudflare_tunnel_id) document.getElementById('admin-cf-tunnel-id').value = s.cloudflare_tunnel_id;
      if (s.tunnel_provider) {
        document.getElementById('admin-server-type').value = s.tunnel_provider;
        adminToggleServerType();
      }
    }).catch(()=>{});
    document.getElementById('admin-login-user').value = '';
    document.getElementById('admin-login-pass').value = '';
    refreshKnownAgents();
    setInterval(refreshKnownAgents, 10000);
  } else {
    document.getElementById('admin-login-error').style.display = 'block';
  }
}

function adminToggleServerType() {
  const val = document.getElementById('admin-server-type').value;
  document.getElementById('admin-cf-fields').style.display = val === 'cloudflare' ? 'block' : 'none';
  document.getElementById('admin-noncf-fields').style.display = val !== 'cloudflare' ? 'block' : 'none';
}

function adminSaveSettings() {
  transportOrder = document.getElementById('admin-transport-priority').value;
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({type:'set_transport_order', order:transportOrder}));
  const repo = document.getElementById('admin-github-repo').value.trim();
  const token = document.getElementById('admin-github-token').value.trim();
  const serverType = document.getElementById('admin-server-type').value;
  const cfTag = document.getElementById('admin-cf-account-tag').value.trim();
  const cfSecret = document.getElementById('admin-cf-tunnel-secret').value.trim();
  const cfHost = document.getElementById('admin-cf-hostname').value.trim();
  const cfID = document.getElementById('admin-cf-tunnel-id').value.trim();
  const serverURL = document.getElementById('admin-server-url').value.trim();
  const authUser = document.getElementById('admin-auth-user').value.trim();
  const authPass = document.getElementById('admin-auth-pass').value.trim();
  const payload = {
    github_repo: repo || undefined,
    github_token: token || undefined,
    tunnel_provider: serverType,
    tunnel_hostname: cfHost || undefined,
    server_url: serverURL || undefined,
    cloudflare_account_tag: cfTag || undefined,
    cloudflare_tunnel_secret: cfSecret || undefined,
    cloudflare_tunnel_id: cfID || undefined,
    auth_user: authUser || undefined,
    auth_pass: authPass || undefined
  };
  fetch('/api/settings', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload)}).catch(()=>{});
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({type:'set_server_type', server_type: serverType}));
    if (cfTag || cfSecret || cfID) {
      ws.send(JSON.stringify({type:'set_cloudflare_credentials', account_tag: cfTag, tunnel_secret: cfSecret, tunnel_id: cfID}));
    }
  }
  alert('Settings saved');
}

function adminLogout() {
  settingsLoggedIn = false;
  authenticated = false;
  remoteControlEnabled = false;
  sessionStorage.removeItem('punmonitor_auth');
  document.querySelectorAll('.auth-gated').forEach(el => el.classList.remove('visible'));
  updateCellActions();
  updateControlBar();
  updateCellHighlights();
  document.getElementById('admin-login-section').style.display = 'block';
  document.getElementById('admin-settings-section').style.display = 'none';
  location.href = '/';
}

function toggleServerType() {
  const val = document.getElementById('server-type').value;
  document.getElementById('cf-fields').style.display = val === 'cloudflare' ? 'block' : 'none';
  document.getElementById('noncf-fields').style.display = val !== 'cloudflare' ? 'block' : 'none';
}

function loginToSettings() {
  const user = document.getElementById('login-user').value.trim() || 'admin';
  const pass = document.getElementById('login-pass').value.trim() || 'puneet12';
  const authUser = document.getElementById('auth-user').value.trim() || 'puneet';
  const authPass = document.getElementById('auth-pass').value.trim() || 'puneet12';
  if (user === authUser && pass === authPass) {
    settingsLoggedIn = true;
    authenticated = true;
    remoteControlEnabled = false;
    sessionStorage.setItem('punmonitor_auth', '1');
    toggleModal('login-modal');
    document.querySelectorAll('.auth-gated').forEach(el => el.classList.add('visible'));
    updateCellActions();
    updateControlBar();
    const returnTo = document.getElementById('login-modal').dataset.return;
    document.getElementById('login-modal').dataset.return = '';
    if (returnTo === 'control') {
      // just auth, no settings modal
    } else {
      fetch('/api/settings').then(r=>r.json()).then(s => {
        if (s.github_repo) document.getElementById('github-repo').value = s.github_repo;
        if (s.auth_user) document.getElementById('auth-user').value = s.auth_user;
        if (s.auth_pass) document.getElementById('auth-pass').value = s.auth_pass;
        if (s.server_url) document.getElementById('server-url').value = s.server_url;
        if (s.tunnel_hostname) document.getElementById('cf-hostname').value = s.tunnel_hostname;
        if (s.cloudflare_account_tag) document.getElementById('cloudflare-account-tag').value = s.cloudflare_account_tag;
        if (s.cloudflare_tunnel_secret) document.getElementById('cloudflare-tunnel-secret').value = s.cloudflare_tunnel_secret;
        if (s.cloudflare_tunnel_id) document.getElementById('cloudflare-tunnel-id').value = s.cloudflare_tunnel_id;
        if (s.tunnel_provider) {
          document.getElementById('server-type').value = s.tunnel_provider;
          toggleServerType();
        }
        // SSH fields
        if (typeof s.ssh_enabled !== 'undefined') document.getElementById('ssh-enabled').checked = s.ssh_enabled;
        if (s.ssh_port) document.getElementById('ssh-port').value = s.ssh_port;
        if (s.ssh_user) document.getElementById('ssh-user').value = s.ssh_user;
        // Fetch password and keys separately (sensitive data)
        fetch('/api/ssh-info').then(r=>r.json()).then(ssh => {
          if (ssh.password) {
            document.getElementById('ssh-password').value = ssh.password;
            document.getElementById('ssh-password').dataset.placeholder = 'leave empty to keep current';
          }
          if (ssh.authorized_keys) document.getElementById('ssh-auth-keys').value = ssh.authorized_keys;
        }).catch(()=>{});
      }).catch(()=>{});
      toggleModal('settings-modal');
    }
    document.getElementById('login-user').value = '';
    document.getElementById('login-pass').value = '';
  } else {
    document.getElementById('login-error').style.display = 'block';
  }
}

function logoutSettings() {
  settingsLoggedIn = false;
  authenticated = false;
  remoteControlEnabled = false;
  sessionStorage.removeItem('punmonitor_auth');
  document.querySelectorAll('.auth-gated').forEach(el => el.classList.remove('visible'));
  updateCellActions();
  updateControlBar();
  updateCellHighlights();
  toggleModal('settings-modal');
}

function updateCellActions() {
  document.querySelectorAll('.cell-actions').forEach(el => {
    el.style.display = 'flex';
    const promoteBtn = el.querySelector('.btn-promote');
    const hideBtn = el.querySelector('.btn-hide');
    const removeBtn = el.querySelector('.btn-remove');
    if (promoteBtn) { promoteBtn.style.display = ''; promoteBtn.classList.add('visible'); }
    if (hideBtn) { hideBtn.style.display = ''; if (authenticated) hideBtn.classList.add('visible'); else hideBtn.classList.remove('visible'); }
    if (removeBtn) { removeBtn.style.display = ''; if (authenticated) removeBtn.classList.add('visible'); else removeBtn.classList.remove('visible'); }
  });
}

function pushUrlsToAgents() {
  if (!authenticated) return;
  const urls = prompt('Server URLs (comma-separated)', 'ws://127.0.0.1:8181/agent/ws');
  if (!urls || !ws || ws.readyState !== WebSocket.OPEN) return;
  const list = urls.split(',').map(s => s.trim()).filter(Boolean);
  ws.send(JSON.stringify({ type: 'push_urls_all', urls: list }));
  alert('URLs pushed to connected agents');
}

function restartAllAgents() {
  if (!authenticated) return;
  if (!confirm('Restart all connected agents?')) return;
  fetch('/api/restart', { method: 'POST' }).catch(() => {});
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'restart' }));
  }
}

function toggleHideAgent(agentId) {
  if (!authenticated) { alert('Login as admin first'); return; }
  const isHidden = hiddenAgents.has(agentId);
  fetch('/api/hide-agent', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({agent_id: agentId, hide: !isHidden})
  }).then(() => {
    if (!isHidden) {
      hiddenAgents.add(agentId);
    } else {
      hiddenAgents.delete(agentId);
    }
    const rec = agentCells[agentId];
    if (rec && rec.cell) {
      rec.cell.classList.toggle('agent-hidden', !isHidden);
      const badge = rec.cell.querySelector('.cell-badge');
      if (badge) badge.textContent = !isHidden ? 'Hidden' : 'Live';
    }
    syncAgentListFromKnown();
    refreshAgents();
  }).catch(() => {
    alert('Failed to toggle agent visibility');
  });
}

function removeAgent(agentId) {
  if (!authenticated) return;
  if (!confirm('Remove agent "' + agentId + '"? This will stop the agent process.')) return;
  fetch('/api/remove-agent', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({agent_id: agentId})
  }).then(() => {
    if (agentCells[agentId]) {
      agentCells[agentId].cell.remove();
      delete agentCells[agentId];
    }
  });
}

function pushUpdateToAgents() {
  if (!authenticated) return;
  const url = prompt('Enter download URL for new PunMonitor.exe:', '');
  if (!url) return;
  fetch('/api/update', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({url: url})
  }).then(r => {
    if (r.ok) alert('Update pushed to agents!');
    else r.text().then(t => alert('Failed: '+t));
  }).catch(e => alert('Error: '+e.message));
}

function saveCaptureSchedule() {
  const schedule = document.getElementById('schedule-input').value.trim();
  const days = document.getElementById('days-input').value.trim();
  const status = document.getElementById('schedule-status');
  if (schedule && schedule !== '24/7') {
    const re = /^\d{1,2}:\d{2}-\d{1,2}:\d{2}$/;
    if (!re.test(schedule)) {
      status.textContent = '⚠ Format must be HH:MM-HH:MM (e.g. 09:00-17:00) or 24/7';
      status.style.color = '#ef5350';
      return;
    }
  }
  if (days && days !== 'daily') {
    const dayRe = /^(Mon|Tue|Wed|Thu|Fri|Sat|Sun)(-(Mon|Tue|Wed|Thu|Fri|Sat|Sun))?$/;
    const csvRe = /^(Mon|Tue|Wed|Thu|Fri|Sat|Sun)(,(Mon|Tue|Wed|Thu|Fri|Sat|Sun))*$/;
    if (!dayRe.test(days) && !csvRe.test(days)) {
      status.textContent = '⚠ Days must be: Mon-Fri, Mon,Wed,Fri, daily, or empty';
      status.style.color = '#ef5350';
      return;
    }
  }
  fetch('/api/settings', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({capture_schedule: schedule || '24/7', capture_days: days || 'daily'})
  }).then(r => r.json()).then(() => {
    status.textContent = '✓ Saved! Active: ' + (schedule || '24/7') + ' / ' + (days || 'daily');
    status.style.color = '#4caf50';
  }).catch(e => {
    status.textContent = '✗ Failed: ' + e;
    status.style.color = '#ef5350';
  });
}

function toggleModal(id) { document.getElementById(id).classList.toggle('active'); }

function saveSettings() {
  toggleModal('settings-modal');
  transportOrder = document.getElementById('transport-priority').value;
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({type:'set_transport_order', order:transportOrder}));
  const repo = document.getElementById('github-repo').value.trim();
  const token = document.getElementById('github-token').value.trim();
  const serverType = document.getElementById('server-type').value;
  const cfTag = document.getElementById('cloudflare-account-tag').value.trim();
  const cfSecret = document.getElementById('cloudflare-tunnel-secret').value.trim();
  const cfID = document.getElementById('cloudflare-tunnel-id').value.trim();
  const cfHost = document.getElementById('cf-hostname').value.trim();
  const serverURL = document.getElementById('server-url').value.trim();
  const authUser = document.getElementById('auth-user').value.trim();
  const authPass = document.getElementById('auth-pass').value.trim();
  // SSH fields
  const sshEnabled = document.getElementById('ssh-enabled').checked;
  const sshPort = parseInt(document.getElementById('ssh-port').value) || 2222;
  const sshUser = document.getElementById('ssh-user').value.trim() || 'admin';
  const sshKeys = document.getElementById('ssh-auth-keys').value.trim();
  const payload = {
    github_repo: repo || undefined,
    github_token: token || undefined,
    tunnel_provider: serverType,
    tunnel_hostname: cfHost || undefined,
    server_url: serverURL || undefined,
    cloudflare_account_tag: cfTag || undefined,
    cloudflare_tunnel_secret: cfSecret || undefined,
    cloudflare_tunnel_id: cfID || undefined,
    auth_user: authUser || undefined,
    auth_pass: authPass || undefined,
    ssh_enabled: sshEnabled,
    ssh_port: sshPort,
    ssh_user: sshUser,
    ssh_authorized_keys: sshKeys || undefined
  };
  fetch('/api/settings', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload)}).catch(()=>{});
  // Also push cloudflare creds via WS for immediate effect
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({type:'set_server_type', server_type: serverType}));
    if (cfTag || cfSecret || cfID) {
      ws.send(JSON.stringify({type:'set_cloudflare_credentials', account_tag: cfTag, tunnel_secret: cfSecret, tunnel_id: cfID}));
    }
    // SSH update via WS for immediate effect (start/stop server)
    ws.send(JSON.stringify({type:'set_ssh', enabled: sshEnabled, port: sshPort, user: sshUser}));
  }
}

function regenerateSSHPassword() {
  if (!authenticated) { alert('Admin login required'); return; }
  if (!confirm('Regenerate SSH password? Any existing SSH sessions using the old password will be disconnected.')) return;
  fetch('/api/ssh-info?regenerate=1').then(r=>r.json()).then(d => {
    if (d.password) {
      document.getElementById('ssh-password').value = d.password;
      // Re-open modal (it was just closed)
      toggleModal('settings-modal');
      // Re-show as plain text so user can copy
      const el = document.getElementById('ssh-password');
      el.type = 'text';
      el.select();
      try { document.execCommand('copy'); } catch(e) {}
      alert('New SSH password generated and copied to clipboard:\n\n' + d.password + '\n\nClick Save to apply.');
    } else {
      alert('Failed to regenerate: ' + (d.error || 'unknown error'));
      toggleModal('settings-modal');
    }
  }).catch(e => {
    alert('Error: ' + e.message);
    toggleModal('settings-modal');
  });
}

// ── GitHub Push & Compile ──
function pushToGitHub() {
  if (!authenticated) return;
  fetch('/api/push-report', {method:'POST'}).then(r => {
    if (r.ok) alert('Report pushed to GitHub!');
    else r.text().then(t => alert('Failed: '+t));
  }).catch(e => alert('Error: '+e.message));
}

function compileMonthly() {
  if (!authenticated) return;
  fetch('/api/compile-monthly-report').then(r=>r.json()).then(d => {
    alert(d.success ? 'Monthly report compiled: '+d.agentCount+' agents' : 'Failed: '+JSON.stringify(d));
  }).catch(e => alert('Error: '+e.message));
}

function pushCompiledReport() {
  if (!authenticated) return;
  fetch('/api/report/compiled/push', {method:'POST'}).then(r=>r.json()).then(d => {
    alert(d.ok ? 'Compiled report pushed: '+d.filename : 'Failed: '+d.error);
  }).catch(e => alert('Error: '+e.message));
}

function downloadReportCSV() {
  const a = document.createElement('a');
  a.href = '/api/reports/merged';
  a.download = 'punmonitor-compiled-report-' + new Date().toISOString().slice(0,10) + '.xlsx';
  a.click();
}

function downloadReportLegacy() {
  const a = document.createElement('a');
  a.href = '/api/report.csv';
  a.download = 'activity-report-' + new Date().toISOString().slice(0,10) + '.csv';
  a.click();
}

// ── Promote to Server ──
function promoteToServer() {
  if (!authenticated) { alert('Admin login required'); return; }
  if (!confirm('Promote this agent to server mode? It will start broadcasting screens.')) return;
  fetch('/api/promote', {method:'POST'}).then(r=>r.json()).then(d => {
    if (d.status === 'ok') {
      serverMode = true;
      document.getElementById('mode-badge').textContent = 'SERVER';
      alert('Promoted to server mode successfully!');
    }
  }).catch(() => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({type:'promote_to_server'}));
    }
  });
}

// ── Known Agents (Registration) ──
function refreshKnownAgents() {
  fetch('/api/known-agents').then(r => r.json()).then(list => {
    const el = document.getElementById('known-agents-table');
    if (!el) return;
    if (!list || list.length === 0) {
      el.innerHTML = '<div style="padding:8px;text-align:center;color:var(--text3)">No agents registered yet</div>';
      return;
    }
    const rows = list.map(a => {
      const status = a.connected ? '<span style="color:#4caf50">● Online</span>' :
        '<span style="color:#ef5350">● Offline</span>';
      const lastSeen = a.last_seen ? new Date(a.last_seen).toLocaleString() : '—';
      return '<div style="display:flex;justify-content:space-between;align-items:center;padding:4px 6px;border-bottom:1px solid var(--border)">' +
        '<span style="font-weight:600;flex:2;overflow:hidden;text-overflow:ellipsis">'+escapeHtml(a.id)+'</span>' +
        '<span style="flex:1;color:var(--text2)">'+escapeHtml(a.hostname||'—')+'</span>' +
        '<span style="flex:1;font-size:9px;color:var(--text3)">'+escapeHtml(a.local_ip||'')+'</span>' +
        '<span style="flex:0 0 60px;text-align:right">'+status+'</span>' +
        '<span style="flex:0 0 140px;text-align:right;font-size:9px;color:var(--text3)">'+lastSeen+'</span>' +
        '</div>';
    }).join('');
    el.innerHTML = '<div style="font-size:9px;color:var(--text3);display:flex;justify-content:space-between;padding:2px 6px;margin-bottom:4px;border-bottom:1px solid var(--border)">' +
      '<span style="flex:2">Agent ID</span><span style="flex:1">Hostname</span><span style="flex:1">IP</span><span style="flex:0 0 60px;text-align:right">Status</span><span style="flex:0 0 140px;text-align:right">Last Seen</span>' +
      '</div>' + rows;
  }).catch(() => {});
}

// ── File Transfer ──
function updateFileTransferAgents() {
  const sel = document.getElementById('file-transfer-agent');
  if (!sel) return;
  const ids = [...knownAgentIds];
  sel.innerHTML = '<option value="">Select agent…</option>' + ids.map(id =>
    '<option value="'+escapeHtml(id)+'">'+escapeHtml(id)+'</option>'
  ).join('');
}

function uploadSelectedFile() {
  const input = document.getElementById('file-input');
  const sel = document.getElementById('file-transfer-agent');
  const status = document.getElementById('file-transfer-status');
  if (!input.files || !input.files[0]) { status.textContent = 'Please select a file'; return; }
  if (!sel.value) { status.textContent = 'Please select a target agent'; return; }
  const formData = new FormData();
  formData.append('file', input.files[0]);
  formData.append('agentId', sel.value);
  status.textContent = 'Uploading ' + input.files[0].name + '…';
  fetch('/api/upload', { method: 'POST', body: formData })
    .then(r => r.json()).then(d => {
      if (d.status === 'ok') {
        status.textContent = '✅ Sent ' + d.name + ' (' + d.size + ' bytes) to ' + sel.value;
      } else {
        status.textContent = '❌ Upload failed: ' + JSON.stringify(d);
      }
    }).catch(e => {
      status.textContent = '❌ Error: ' + e.message;
    });
}

function promoteAgent(agentId) {
  if (!authenticated) { alert('Admin login required'); return; }
  if (!confirm('Promote "' + agentId + '" to server mode?')) return;
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({type:'promote_to_server', target: agentId}));
  } else {
    alert('WebSocket not connected');
  }
}

// ── Share Link ──
function generateShareLink() {
  const targetAgent = controlledAgentId || activeAgentId || '';
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({type:'generate_share_link', agentId: targetAgent}));
  } else {
    const link = location.origin + '/?agent=' + targetAgent;
    document.getElementById('share-link-output').value = link;
    toggleModal('share-modal');
  }
}

function copyShareLinkModal() {
  const input = document.getElementById('share-link-output');
  input.select();
  input.setSelectionRange(0, 99999);
  navigator.clipboard.writeText(input.value).then(() => {
    const btn = event?.target;
    if (btn) { const orig = btn.textContent; btn.textContent = 'Copied!'; setTimeout(() => btn.textContent = orig, 1500); }
  }).catch(() => {
    document.execCommand('copy');
  });
}

// ── Init ──
document.addEventListener('DOMContentLoaded', () => {
  // Check if setup is needed
  fetch('/api/setup-status').then(r=>r.json()).then(d => {
    if (d.needs_setup && location.pathname !== '/admin') {
      showSetupWizard(d);
    }
  }).catch(()=>{});

  // Fetch version from server and update header
  fetch('/api/version').then(r=>r.json()).then(d => {
    if (!d || !d.version) return;
    window._serverVersion = d.version;
    const vb = document.getElementById('version-badge');
    if (vb) vb.textContent = 'v' + d.version;
    // If the update modal is already open, update the "current" label too
    const cur = document.getElementById('update-current-version');
    if (cur) cur.textContent = d.version;
  }).catch(() => {});

  // Load capture schedule fields
  fetch('/api/settings').then(r=>r.json()).then(d => {
    const schedEl = document.getElementById('schedule-input');
    const daysEl = document.getElementById('days-input');
    if (schedEl && d.capture_schedule !== undefined) schedEl.value = d.capture_schedule || '';
    if (daysEl && d.capture_days !== undefined) daysEl.value = d.capture_days || '';
  }).catch(()=>{});
  if (sessionStorage.getItem('punmonitor_auth') === '1') {
    settingsLoggedIn = true;
    authenticated = true;
    remoteControlEnabled = false;
    document.querySelectorAll('.auth-gated').forEach(el => el.classList.add('visible'));
    updateCellActions();
    updateControlBar();
  }
  document.getElementById('screen-canvas').style.display = 'none';
  document.getElementById('main').classList.add('grid-mode');
  // Restore view mode from localStorage
  var savedView = localStorage.getItem('pm_viewMode');
  if (savedView === 'single') {
    setViewMode('single');
  } else {
    setViewMode('grid');
  }
  // Restore controlled agent from localStorage
  var savedAgent = localStorage.getItem('pm_controlledAgent');
  if (savedAgent) {
    setTimeout(function() { setControlledAgent(savedAgent, false); }, 1000);
  }
  // Restore active tab (only "dashboard" is now exposed; stored value is
  // ignored so legacy 'audit' values don't break anything).
  try { localStorage.removeItem('pm_active_tab'); } catch (e) {} // legacy localStorage key — safe to keep, just clears stale state
  // Audit data is still available via /api/audit and the XLSX report.
  // Background refresh keeps /api/audit warm and the XLSX report current.
  setInterval(function() {
    fetch('/api/audit').then(r=>r.json()).then(entries => {
      allAuditEntries = (entries || []).slice().reverse();
    }).catch(()=>{});
  }, 60000);
  refreshElectionStatus();
  setInterval(refreshElectionStatus, 15000);
  if (location.pathname === '/admin') {
    document.getElementById('admin-page').classList.add('active');
    document.getElementById('topbar').style.display = 'none';
    document.getElementById('app').style.display = 'none';
    if (settingsLoggedIn) {
      document.getElementById('admin-login-section').style.display = 'none';
      document.getElementById('admin-settings-section').style.display = 'block';
      document.querySelectorAll('.auth-gated').forEach(el => el.classList.add('visible'));
    }
    return;
  }
  connect();
  if (location.pathname.includes('server') || location.search.includes('server')) serverMode = true;
  document.getElementById('mode-badge').textContent = serverMode ? 'SERVER' : 'AGENT';
  refreshAgents();
  setInterval(refreshAgents, 5000);
  startMainCanvasRefresher();
  setInterval(refreshServerLoad, 5000);
  // Show sidebar buttons after auth
  if (authenticated) {
    document.querySelectorAll('.sidebar-btn').forEach(function(el) {
      el.style.display = 'flex';
    });
  }
  // Grid Zoom Slider
  const zoomSlider = document.getElementById('grid-zoom-slider');
  if (zoomSlider) {
    const savedZoom = localStorage.getItem('pm_grid_zoom');
    if (savedZoom) {
      zoomSlider.value = savedZoom;
      document.documentElement.style.setProperty('--grid-min-width', savedZoom + 'px');
    }
    zoomSlider.addEventListener('input', (e) => {
      const val = e.target.value;
      document.documentElement.style.setProperty('--grid-min-width', val + 'px');
      localStorage.setItem('pm_grid_zoom', val);
    });
  }
});

var assistSessions = {};

function downloadAgent() {
  window.location.href = '/api/agent/download';
}

function openAssistSession() {
  fetch('/assist/new', {method:'POST'}).then(function(r){return r.json()}).then(function(d) {
    var url = location.origin + d.url;
    // Copy link
    navigator.clipboard.writeText(url).then(function() {
      var btn = document.querySelector('[onclick="openAssistSession()"]');
      if (btn) { var orig = btn.textContent; btn.textContent = '✓ Link Copied!'; setTimeout(function(){ btn.textContent = orig; }, 2000); }
    }).catch(function(){});
    // Create assist cell in grid
    createAssistCell(d.id, url);
  }).catch(function(e) { alert('Failed: ' + e); });
}

function closeAssistSession(sessionId, agentId) {
  if (assistSessions[sessionId]) {
    try { assistSessions[sessionId].ws.close(); } catch(e) {}
    delete assistSessions[sessionId];
  }
  if (agentCells[agentId]) {
    if (agentCells[agentId].cell && agentCells[agentId].cell.parentNode) {
      agentCells[agentId].cell.remove();
    }
    delete agentCells[agentId];
  }
  fetch('/api/assist-close', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({session_id: sessionId})
  }).catch(function(){});
}

function createAssistCell(sessionId, userUrl) {
  var agentId = 'assist-' + sessionId;
  if (agentCells[agentId]) return;
  ensureAgentCell(agentId);
  var rec = agentCells[agentId];
  if (rec.nameEl) rec.nameEl.textContent = '🖥 ' + sessionId;
  if (rec.hostnameEl) rec.hostnameEl.textContent = 'Remote Assist';
  if (rec.badgeEl) { rec.badgeEl.textContent = 'Waiting...'; rec.badgeEl.className = 'cell-badge'; rec.badgeEl.style.background = 'rgba(255,193,7,.2)'; rec.badgeEl.style.color = '#ffc107'; }
  if (rec.cellModeEl) { rec.cellModeEl.textContent = 'ASSIST'; rec.cellModeEl.style.background = 'rgba(76,175,80,.2)'; rec.cellModeEl.style.color = '#4caf50'; }

  // Manual close button (no auto-removal)
  var actionsEl = rec.cell.querySelector('.cell-actions');
  if (actionsEl) {
    var focusBtn = document.createElement('button');
    focusBtn.innerHTML = '⛶';
    focusBtn.title = 'Focus this assist session (fullscreen)';
    focusBtn.style.cssText = 'background:none;border:none;cursor:pointer;font-size:12px;padding:2px 6px;color:var(--primary)';
    focusBtn.onclick = function(e) {
      e.stopPropagation();
      focusAgent(agentId);
    };
    actionsEl.appendChild(focusBtn);

    var closeBtn = document.createElement('button');
    closeBtn.innerHTML = '✕';
    closeBtn.title = 'Close assist session';
    closeBtn.style.cssText = 'background:none;border:none;cursor:pointer;font-size:12px;padding:2px 6px;color:#ef5350';
    closeBtn.onclick = function(e) {
      e.stopPropagation();
      closeAssistSession(sessionId, agentId);
    };
    actionsEl.appendChild(closeBtn);
  }

  // Add share link and viewer link below cell
  var infoEl = rec.cell.querySelector('.cell-info');
  if (infoEl) {
    var linkDiv = document.createElement('div');
    linkDiv.style.cssText = 'padding:4px 8px;font-size:10px;display:flex;gap:4px;flex-wrap:wrap';
    linkDiv.innerHTML = '<span style="color:var(--text3)">Share:</span><a href="' + userUrl + '" target="_blank" style="color:#4caf50;text-decoration:none;word-break:break-all">' + userUrl + '</a>';
    infoEl.parentNode.insertBefore(linkDiv, infoEl.nextSibling);
  }

  // Connect via WebSocket to receive frames
  var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  var wsUrl = proto + '//' + location.host + '/assist-ws/' + sessionId;
  var assistWs = new WebSocket(wsUrl);
  assistSessions[sessionId] = { ws: assistWs, canvas: rec.canvas, agentId: agentId };

  assistWs.onopen = function() {
    assistWs.send(JSON.stringify({type:'assist_admin_view', sessionId: sessionId}));
    if (rec.badgeEl) { rec.badgeEl.textContent = 'Connected'; rec.badgeEl.style.background = 'rgba(76,175,80,.2)'; rec.badgeEl.style.color = '#4caf50'; }
  };

  assistWs.onmessage = function(e) {
    try {
      var msg = JSON.parse(e.data);
      if (msg.type === 'assist_frame' && msg.data) {
        // Store the frame data so the main canvas can render it when focused
        rec.lastB64 = msg.data;
        rec.lastPayload = { data: msg.data };
        var canvas = rec.canvas;
        var ctx = canvas.getContext('2d');
        var img = new Image();
        img.onload = function() {
          canvas.width = img.width;
          canvas.height = img.height;
          ctx.drawImage(img, 0, 0);
          rec.emptyEl.style.display = 'none';
          canvas.style.display = 'block';
        };
        img.src = 'data:image/jpeg;base64,' + msg.data;
        if (rec.badgeEl) rec.badgeEl.textContent = 'Live';
      }
      if (msg.type === 'assist_chat') {
        // Show chat notification
        if (rec.badgeEl) rec.badgeEl.textContent = '💬 Chat';
      }
    } catch(err) {}
  };

  assistWs.onclose = function() {
    if (rec.badgeEl) { rec.badgeEl.textContent = 'Disconnected'; rec.badgeEl.style.background = 'rgba(255,0,0,.2)'; rec.badgeEl.style.color = '#ef5350'; }
  };

  // Make cell clickable for focus view
  rec.cell.style.cursor = 'pointer';
  rec.cell.title = 'Click to focus this assist session';
  rec.cell.addEventListener('click', function(e) {
    if (e.target.closest('button')) return;
    focusAgent(agentId);
  });
}

function sendAssistChat(sessionId, text) {
  var s = assistSessions[sessionId];
  if (s && s.ws && s.ws.readyState === WebSocket.OPEN) {
    s.ws.send(JSON.stringify({type:'assist_chat', sessionId: sessionId, sender:'admin', text: text}));
  }
}

// ── Terminal ──
let termAgentId = null;
function openTerminal(agentId) {
  termAgentId = agentId;
  const panel = document.getElementById('terminal-panel');
  panel.style.display = 'flex';
  document.getElementById('term-title').textContent = 'Terminal — ' + agentId;
  document.getElementById('term-input').focus();
}
function closeTerminal() { document.getElementById('terminal-panel').style.display = 'none'; }
function termExec() {
  const input = document.getElementById('term-input');
  const cmd = input.value.trim();
  if (!cmd || !termAgentId) return;
  const id = 'term-' + Date.now();
  document.getElementById('term-output').innerHTML += '<div style="color:#4caf50">$ ' + escapeHtml(cmd) + '</div>';
  input.value = '';
  fetch('/api/terminal', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({id, command: cmd, agentId: termAgentId})});
  window._termCallbacks = window._termCallbacks || {};
  window._termCallbacks[id] = function(output) {
    document.getElementById('term-output').innerHTML += '<div>' + escapeHtml(output) + '</div>';
    document.getElementById('term-output').scrollTop = 99999;
  };
}
function handleCommandOutput(d) {
  if (d.id && window._termCallbacks && window._termCallbacks[d.id]) {
    window._termCallbacks[d.id](d.output);
    delete window._termCallbacks[d.id];
  }
}

// ── File Manager ──
let fmAgentId = null, fmCurrentPath = '';
function openFileManager(agentId) {
  fmAgentId = agentId;
  document.getElementById('fm-panel').style.display = 'flex';
  document.getElementById('fm-title').textContent = 'Files — ' + agentId;
  fmNavigate('/');
}
function closeFileManager() { document.getElementById('fm-panel').style.display = 'none'; }
function fmNavigate(path) {
  fmCurrentPath = path;
  document.getElementById('fm-path').textContent = path;
  const id = 'fm-' + Date.now();
  fetch('/api/files/list', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({id, path, agentId: fmAgentId})});
  window._fmCallbacks = window._fmCallbacks || {};
  window._fmCallbacks[id] = function(entries) { fmRender(entries, path); };
}
function fmRender(entries, basePath) {
  const list = document.getElementById('fm-list');
  list.innerHTML = '';
  if (!entries || !entries.length) { list.innerHTML = '<div style="padding:8px;color:var(--text3)">Empty</div>'; return; }
  entries.forEach(e => {
    const row = document.createElement('div');
    row.style.cssText = 'display:flex;align-items:center;padding:4px 8px;cursor:pointer;border-bottom:1px solid rgba(255,255,255,.05);font-size:12px';
    row.onmouseover = () => row.style.background = 'rgba(255,255,255,.05)';
    row.onmouseout = () => row.style.background = '';
    if (e.isDir) {
      row.innerHTML = '<span style="width:20px;text-align:center">📁</span><span style="flex:1;overflow:hidden;text-overflow:ellipsis">' + escapeHtml(e.name) + '</span>';
      row.onclick = () => {
        const sep = (basePath.endsWith('/') || basePath.endsWith('\\') || e.name === '..') ? '' : (basePath.includes('\\') ? '\\' : '/');
        fmNavigate(basePath + sep + e.name);
      };
    } else {
      const sizeStr = e.size > 1048576 ? (e.size/1048576).toFixed(1)+'MB' : e.size > 1024 ? (e.size/1024).toFixed(1)+'KB' : e.size+'B';
      row.innerHTML = '<span style="width:20px;text-align:center">📄</span><span style="flex:1;overflow:hidden;text-overflow:ellipsis">' + escapeHtml(e.name) + '</span><span style="color:var(--text3);margin-left:8px">' + sizeStr + '</span>';
      row.onclick = () => {
        const full = (basePath.endsWith('/') || basePath.endsWith('\\')) ? basePath + e.name : basePath + (basePath.includes('\\') ? '\\' : '/') + e.name;
        fmDownload(full, e.name);
      };
    }
    list.appendChild(row);
  });
}
function fmDownload(path, name) {
  const id = 'fmd-' + Date.now();
  fetch('/api/files/download', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({id, path, agentId: fmAgentId})});
  window._fmCallbacks = window._fmCallbacks || {};
  window._fmCallbacks[id] = function(d) {
    if (d.error) { alert('Download error: ' + d.error); return; }
    if (d.data) {
      const bytes = atob(d.data);
      const arr = new Uint8Array(bytes.length);
      for (let i = 0; i < bytes.length; i++) arr[i] = bytes.charCodeAt(i);
      const blob = new Blob([arr]);
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = d.name || name;
      a.click();
    }
  };
}
function handleDirListing(d) {
  if (d.id && window._fmCallbacks && window._fmCallbacks[d.id]) {
    window._fmCallbacks[d.id](d.entries || []);
    delete window._fmCallbacks[d.id];
  }
}
function handleFileDownloadDash(d) {
  if (d.id && window._fmCallbacks && window._fmCallbacks[d.id]) {
    window._fmCallbacks[d.id](d);
    delete window._fmCallbacks[d.id];
  }
}

// switchTab removed in v10.0.26 — dashboard is the only view, no tab navigation needed.
// Audit data is available via the XLSX report (header button) and /api/audit endpoint.
function refreshAuditLog() {
  fetch('/api/audit').then(r=>r.json()).then(entries => {
    const el = document.getElementById('audit-list');
    if (!entries || !entries.length) {
      if (el) el.innerHTML = '<div style="padding:8px;color:var(--text3);font-size:11px">No audit entries</div>';
      allAuditEntries = [];
      renderAuditPage();
      return;
    }
    if (el) el.innerHTML = '';
    allAuditEntries = entries.slice().reverse();
    const c = document.getElementById('audit-count');
    if (c) c.textContent = allAuditEntries.length;
    renderAuditPage();
  }).catch(()=>{});
}
function renderAuditPage() {
  const body = document.getElementById('audit-table-body');
  const stats = document.getElementById('audit-stats');
  if (!body) return;
  const search = (document.getElementById('audit-search')?.value || '').toLowerCase();
  const actionFilter = document.getElementById('audit-action-filter')?.value || '';
  const hours = parseInt(document.getElementById('audit-time-filter')?.value || '0', 10);
  const cutoff = hours > 0 ? Date.now() - hours * 3600 * 1000 : 0;
  const filtered = allAuditEntries.filter(e => {
    if (actionFilter && e.action !== actionFilter) return false;
    if (cutoff > 0 && e.ts < cutoff) return false;
    if (search) {
      const hay = (e.action + ' ' + (e.agentId||'') + ' ' + (e.user||'') + ' ' + (e.detail||'')).toLowerCase();
      if (!hay.includes(search)) return false;
    }
    return true;
  });
  if (filtered.length === 0) {
    body.innerHTML = '<div style="padding:24px;text-align:center;color:var(--text3);font-size:12px">No entries match the current filter</div>';
  } else {
    const rows = filtered.slice(0, 1000).map(e => {
      const d = new Date(e.ts);
      const ts = d.getFullYear() + '-' + String(d.getMonth()+1).padStart(2,'0') + '-' + String(d.getDate()).padStart(2,'0') + ' ' +
                 String(d.getHours()).padStart(2,'0') + ':' + String(d.getMinutes()).padStart(2,'0') + ':' + String(d.getSeconds()).padStart(2,'0');
      const cls = 'act-' + (e.action || 'default');
      return '<div>' +
             '<div style="color:var(--text3);font-family:monospace">' + ts + '</div>' +
             '<div><span class="action-chip ' + cls + '">' + escapeHtml(e.action) + '</span></div>' +
             '<div style="color:var(--text2);overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + escapeHtml(e.agentId||'') + '">' + escapeHtml(e.agentId||'—') + '</div>' +
             '<div style="color:var(--text3)">' + escapeHtml(e.user||'—') + '</div>' +
             '<div style="color:var(--text3);overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + escapeHtml(e.detail||'') + '">' + escapeHtml(e.detail||'') + '</div>' +
             '</div>';
    }).join('');
    body.innerHTML = rows;
  }
  if (stats) {
    const showing = Math.min(filtered.length, 1000);
    const total = allAuditEntries.length;
    stats.textContent = 'Showing ' + showing + ' of ' + filtered.length + ' filtered (' + total + ' total). Download XLSX for full history.';
  }
}

// ── Server Migration ──
function openMigration() {
  document.getElementById('migrate-panel').style.display = 'flex';
  document.getElementById('migrate-url').value = cfgData.server_url || '';
}
function closeMigration() { document.getElementById('migrate-panel').style.display = 'none'; }
var cfgData = {};
function loadConfigForMigration() {
  fetch('/api/settings').then(r=>r.json()).then(d => { cfgData = d; }).catch(()=>{});
}
function doMigrate() {
  const url = document.getElementById('migrate-url').value.trim();
  if (!url) return alert('Enter server URL');
  if (!confirm('Migrate ALL agents to ' + url + '?')) return;
  fetch('/api/migrate', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({server_url: url})})
    .then(r=>r.json()).then(d => { alert(d.msg || 'Done'); closeMigration(); }).catch(e => alert('Error: ' + e));
}
loadConfigForMigration();

// ── Enhanced handleMessage for terminal/file/audit ──
const _origHandleMessage = typeof handleMessage === 'function' ? handleMessage : null;
const origHandleMessage = handleMessage;
handleMessage = function(d) {
  if (d.type === 'command_output') { handleCommandOutput(d); return; }
  if (d.type === 'dir_listing') { handleDirListing(d); return; }
  if (d.type === 'file_download' && d.data) { handleFileDownloadDash(d); return; }
  if (origHandleMessage) origHandleMessage(d);
};

function showSetupWizard(info) {
  document.getElementById('setup-overlay').style.display = 'flex';
  document.getElementById('app').style.display = 'none';
  if (info && info.hostname) {
    document.getElementById('sw-server').placeholder = 'http://' + info.hostname + ':8080';
  }
  // Pre-fill from current settings (most of which come from GitHub sync)
  fetch('/api/settings').then(r=>r.json()).then(s => {
    if (s.server_url) document.getElementById('sw-server').value = s.server_url;
    if (s.auth_user) document.getElementById('sw-user').value = s.auth_user;
    if (s.auth_pass) document.getElementById('sw-pass').value = s.auth_pass;
    if (s.tunnel_hostname) document.getElementById('sw-tunnel-host').value = s.tunnel_hostname;
    if (s.cloudflare_account_tag) document.getElementById('sw-cf-tag').value = s.cloudflare_account_tag;
    if (s.cloudflare_tunnel_id) document.getElementById('sw-cf-id').value = s.cloudflare_tunnel_id;
    if (s.cloudflare_tunnel_secret) document.getElementById('sw-cf-secret').value = s.cloudflare_tunnel_secret;
  }).catch(()=>{});
}
function skipSetup() {
  document.getElementById('setup-overlay').style.display = 'none';
  document.getElementById('app').style.display = '';
  connect();
}
function doSetup() {
  var serverUrl = document.getElementById('sw-server').value.trim();
  var user = document.getElementById('sw-user').value.trim();
  var pass = document.getElementById('sw-pass').value;
  var fps = parseFloat(document.getElementById('sw-fps').value) || 1;
  if (!serverUrl) {
    document.getElementById('sw-msg').style.display = 'block';
    document.getElementById('sw-msg').textContent = 'Server URL is required';
    return;
  }
  var body = {
    server_url: serverUrl,
    auth_user: user,
    auth_pass: pass,
    max_fps: fps,
    tunnel_hostname: document.getElementById('sw-tunnel-host').value.trim(),
    cloudflare_tunnel_id: document.getElementById('sw-cf-id').value.trim(),
    cloudflare_tunnel_secret: document.getElementById('sw-cf-secret').value,
    cloudflare_account_tag: document.getElementById('sw-cf-tag').value.trim()
  };
  document.getElementById('sw-msg').style.display = 'none';
  fetch('/api/setup', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)})
    .then(r=>r.json()).then(d => {
      if (d.status === 'ok') {
        document.getElementById('sw-msg').style.display = 'block';
        document.getElementById('sw-msg').style.color = '#4caf50';
        document.getElementById('sw-msg').textContent = 'Setup complete! Restart the application to apply.';
        setTimeout(function() { document.getElementById('setup-overlay').style.display = 'none'; document.getElementById('app').style.display = ''; connect(); }, 3000);
      } else {
        document.getElementById('sw-msg').style.display = 'block';
        document.getElementById('sw-msg').textContent = d.msg || 'Setup failed';
      }
    }).catch(e => {
      document.getElementById('sw-msg').style.display = 'block';
      document.getElementById('sw-msg').textContent = 'Error: ' + e;
    });
}

// ── SSH info card ────────────────────────────────────────────────
let sshInfoCache = null;
async function refreshSshInfo() {
  try {
    const r = await fetch('/api/ssh-info');
    if (!r.ok) return;
    sshInfoCache = await r.json();
    const badge = document.getElementById('ssh-badge');
    if (badge) {
      if (sshInfoCache.enabled) {
        badge.textContent = `ssh -p ${sshInfoCache.port} ${sshInfoCache.user}@${sshInfoCache.host}`;
        badge.style.color = '#9eff9e';
      } else {
        badge.textContent = 'ssh: off';
        badge.style.color = 'rgba(255,255,255,.5)';
      }
    }
  } catch (e) {}
}

function openSshModal() {
  if (!sshInfoCache) { refreshSshInfo().then(showSshModal); return; }
  showSshModal();
}

function showSshModal() {
  if (!sshInfoCache) return;
  const d = sshInfoCache;
  const overlay = document.createElement('div');
  overlay.style.cssText = 'position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,.7);z-index:10000;display:flex;align-items:center;justify-content:center';
  overlay.onclick = function(e) { if (e.target === overlay) overlay.remove(); };
  const features = (d.features || []).map(function(f){ return '<span class="ssh-feat">' + f + '</span>'; }).join('');
  overlay.innerHTML = `
    <div class="auth-gated" style="background:#1a1d23;color:#e0e0e0;padding:24px;border-radius:12px;width:560px;max-width:92vw;box-shadow:0 20px 60px rgba(0,0,0,.6);font-family:system-ui">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:16px">
        <h2 style="margin:0;font-size:18px">🔐 SSH Access</h2>
        <button onclick="this.closest('div[style*=\"position:fixed\"]').remove()" style="background:transparent;border:none;color:#fff;font-size:22px;cursor:pointer">×</button>
      </div>
      <div style="background:rgba(255,255,255,.05);padding:12px;border-radius:8px;margin-bottom:12px">
        <div style="font-size:11px;opacity:.6;margin-bottom:4px">Status</div>
        <div style="font-size:13px">${d.enabled ? '✅ <b style="color:#9eff9e">Enabled</b>' : '❌ <b style="color:#ff9e9e">Disabled</b>'}</div>
        <div style="font-size:11px;opacity:.6;margin:8px 0 2px">Features</div>
        <div style="display:flex;flex-wrap:wrap;gap:4px">${features}</div>
      </div>
      <div style="background:rgba(255,255,255,.05);padding:12px;border-radius:8px;margin-bottom:12px">
        <div style="font-size:11px;opacity:.6;margin-bottom:4px">Host Fingerprint</div>
        <div style="font-family:monospace;font-size:11px;word-break:break-all">${d.fingerprint || '—'}</div>
      </div>
      <div style="background:rgba(255,255,255,.05);padding:12px;border-radius:8px;margin-bottom:12px">
        <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px">
          <span style="font-size:11px;opacity:.6">SSH Command</span>
          <button class="ssh-copy" data-cmd="${d.ssh_cmd}" style="background:#2c7;border:none;color:#fff;padding:3px 8px;border-radius:4px;cursor:pointer;font-size:10px">Copy</button>
        </div>
        <code style="font-family:monospace;font-size:12px;display:block;background:rgba(0,0,0,.3);padding:6px;border-radius:4px;word-break:break-all">${d.ssh_cmd}</code>
      </div>
      <div style="background:rgba(255,255,255,.05);padding:12px;border-radius:8px;margin-bottom:12px">
        <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px">
          <span style="font-size:11px;opacity:.6">SFTP Command</span>
          <button class="ssh-copy" data-cmd="${d.sftp_cmd}" style="background:#2c7;border:none;color:#fff;padding:3px 8px;border-radius:4px;cursor:pointer;font-size:10px">Copy</button>
        </div>
        <code style="font-family:monospace;font-size:12px;display:block;background:rgba(0,0,0,.3);padding:6px;border-radius:4px;word-break:break-all">${d.sftp_cmd}</code>
      </div>
      <div style="background:rgba(255,255,255,.05);padding:12px;border-radius:8px;margin-bottom:12px">
        <div style="font-size:11px;opacity:.6;margin-bottom:4px">Username / Password</div>
        <div style="font-family:monospace;font-size:12px">user: <b>${d.user || '—'}</b></div>
        <div style="font-family:monospace;font-size:12px;display:flex;align-items:center;gap:8px;margin-top:4px">pass: <b id="ssh-pass-display">••••••••••••</b>
          <button id="ssh-show-pass" data-pass="${d.password || ''}" style="background:#555;border:none;color:#fff;padding:2px 6px;border-radius:3px;cursor:pointer;font-size:9px">Show</button>
          <button class="ssh-copy" data-cmd="${d.password || ''}" style="background:#2c7;border:none;color:#fff;padding:2px 6px;border-radius:3px;cursor:pointer;font-size:9px">Copy</button>
        </div>
      </div>
      <div style="font-size:10px;opacity:.5;text-align:center;margin-top:8px">All SSH events are recorded in the audit log</div>
    </div>`;
  document.body.appendChild(overlay);
  // Wire up copy buttons
  overlay.querySelectorAll('.ssh-copy').forEach(function(b) {
    b.onclick = function(e) {
      e.stopPropagation();
      navigator.clipboard.writeText(b.getAttribute('data-cmd'));
      const orig = b.textContent;
      b.textContent = '✓ Copied';
      setTimeout(function(){ b.textContent = orig; }, 1200);
    };
  });
  // Show/hide password
  const showPassBtn = overlay.querySelector('#ssh-show-pass');
  if (showPassBtn) {
    showPassBtn.onclick = function(e) {
      e.stopPropagation();
      const disp = overlay.querySelector('#ssh-pass-display');
      if (showPassBtn.textContent === 'Show') {
        disp.textContent = showPassBtn.getAttribute('data-pass');
        showPassBtn.textContent = 'Hide';
      } else {
        disp.textContent = '••••••••••••';
        showPassBtn.textContent = 'Show';
      }
    };
  }
}

// Refresh SSH info on load + every 30s
setTimeout(refreshSshInfo, 1500);
setInterval(refreshSshInfo, 30000);

// ── Transport status badge (WebRTC / QUIC / WS / GitHub) ──
let transportCache = null;
async function refreshTransportInfo() {
  try {
    const r = await fetch('/api/transport-status');
    if (!r.ok) return;
    const d = await r.json();
    transportCache = d;
    const el = document.getElementById('transport-badge');
    if (!el) return;
    // Map priority to a "preferred" label
    let label = d.active || 'none';
    if (d.active_priority === 1) label = 'webrtc ⭐'; // ⭐ = preferred for screen data
    if (d.active === 'quic') label = 'quic';
    if (d.active === 'github') label = 'github';
    if (d.active === 'websocket' || d.active === 'ws') label = 'ws';
    el.textContent = `transport: ${label}`;
    // Color: green if healthy, yellow if degraded, red if dead
    if (d.healthy) {
      el.style.color = '#9eff9e';
    } else {
      el.style.color = '#ff9e9e';
    }
    // Update the on-screen quality overlay (latency / loss / transport)
    updateQualityOverlay({
      avg_latency_ms: d.avg_latency_ms,
      packet_loss_pct: d.packet_loss_pct,
      jitter_ms: d.jitter_ms,
      transport: label,
      live_agents: d.live_agents
    });
  } catch(e) { /* ignore */ }
}
function openTransportModal() {
  if (!transportCache) { refreshTransportInfo().then(showTransportModal); return; }
  showTransportModal();
}
function showTransportModal() {
  if (!transportCache) return;
  const d = transportCache;
  const allTransports = (d.all_transports || []).map(t => {
    const star = t.priority === 1 ? ' ⭐ preferred for screen data' : '';
    const color = t.healthy ? '#9eff9e' : '#ff9e9e';
    return `<li><b style="color:${color}">${t.name}</b> &nbsp; priority ${t.priority}${star} &nbsp; ${t.healthy ? 'healthy' : 'DOWN'}</li>`;
  }).join('');
  const overlay = document.createElement('div');
  overlay.style.cssText = 'position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,.7);z-index:10000;display:flex;align-items:center;justify-content:center';
  overlay.onclick = function(e) { if (e.target === overlay) overlay.remove(); };
  overlay.innerHTML = `
    <div style="background:#1a1d23;color:#e0e0e0;padding:24px;border-radius:12px;width:560px;max-width:92vw;box-shadow:0 20px 60px rgba(0,0,0,.6);font-family:system-ui">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:16px">
        <h2 style="margin:0;font-size:18px">📡 Transport Status</h2>
        <button onclick="this.closest('div[style*=\"position:fixed\"]').remove()" style="background:transparent;border:none;color:#fff;font-size:22px;cursor:pointer">×</button>
      </div>
      <div style="background:rgba(255,255,255,.05);padding:12px;border-radius:8px;margin-bottom:12px">
        <p style="margin:6px 0"><b>Active:</b> <span style="font-family:monospace;color:${d.healthy?'#9eff9e':'#ff9e9e'}">${d.active || 'none'}</span> (priority ${d.active_priority})</p>
        <p style="margin:6px 0"><b>WebSocket clients:</b> ${d.ws_clients}</p>
        <p style="margin:6px 0"><b>QUIC agents connected:</b> ${d.quic_agents}</p>
        <p style="margin:6px 0"><b>QUIC server port:</b> ${d.quic_server_port} (UDP)</p>
        <p style="margin:6px 0"><b>Tunnel:</b> ${d.tunnel_type} (${d.tunnel_active ? 'active' : 'inactive'})</p>
        <p style="margin:6px 0"><b>Live agents:</b> ${d.live_agents||0} / ${d.total_agents||0}</p>
        <p style="margin:6px 0"><b>Avg latency:</b> ${(d.avg_latency_ms||0) > 0 ? Math.round(d.avg_latency_ms) + 'ms' : '—'}</p>
        <p style="margin:6px 0"><b>Total bandwidth:</b> ${(() => { const bps = d.total_bandwidth_bps || 0; if (bps > 1048576) return (bps/1048576).toFixed(2) + ' MB/s'; if (bps > 1024) return (bps/1024).toFixed(1) + ' KB/s'; return Math.round(bps) + ' B/s'; })()}</p>
      </div>
      <h3 style="margin:14px 0 8px">All registered transports</h3>
      <ul style="margin:0;padding-left:20px;line-height:1.7">${allTransports || '<li>none</li>'}</ul>
      <hr style="margin:14px 0;border:0;border-top:1px solid #444">
      <p style="margin:6px 0;font-size:12px;color:#888;line-height:1.5">
        <b>WebRTC</b> (priority 1) is preferred for screen data because it's peer-to-peer — the server doesn't have to re-broadcast frames to viewers, cutting CPU/bandwidth by ~50% for 50+ concurrent agents.<br><br>
        <b>QUIC</b> (UDP/443) is the second choice — works through most NATs without port forwarding.<br><br>
        <b>WebSocket</b> is the fallback for control messages and signaling.<br><br>
        <b>GitHub</b> is the last-resort transport (polling the API, very slow but works anywhere).
      </p>
    </div>
  `;
  document.body.appendChild(overlay);
}
setTimeout(refreshTransportInfo, 2000);
setInterval(refreshTransportInfo, 10000);

// ── Picture-in-Picture (detach the focused canvas into a floating window) ──
// Browser PiP API only supports <video> elements, so we capture the canvas
// via captureStream() and route it into a hidden <video> element.
let _pipVideoEl = null;
let _pipStream = null;
async function togglePip() {
  const canvas = document.getElementById('screen-canvas');
  const btn = document.getElementById('btn-pip');
  if (!canvas) return;
  // Exit PiP if currently in it
  if (document.pictureInPictureElement) {
    try { await document.exitPictureInPicture(); } catch(e) {}
    return;
  }
  try {
    if (!_pipVideoEl) {
      _pipVideoEl = document.createElement('video');
      _pipVideoEl.id = 'pip-video';
      _pipVideoEl.muted = true;
      _pipVideoEl.playsInline = true;
      _pipVideoEl.style.cssText = 'position:absolute;width:1px;height:1px;opacity:0;pointer-events:none;left:-9999px';
      document.body.appendChild(_pipVideoEl);
    }
    // Get a live stream from the canvas (15fps is plenty for a small floating preview)
    if (_pipStream) { try { _pipStream.getTracks().forEach(t => t.stop()); } catch(e) {} }
    _pipStream = canvas.captureStream(15);
    _pipVideoEl.srcObject = _pipStream;
    await _pipVideoEl.play();
    await _pipVideoEl.requestPictureInPicture();
    if (btn) { btn.textContent = '📺 Exit PiP'; btn.classList.add('active'); }
  } catch(err) {
    if (btn) btn.textContent = '📺 PiP';
    alert('Picture-in-Picture not supported or blocked by browser. Use Chrome/Edge for best results.\n\nError: ' + err.message);
  }
}
if (document.pictureInPictureEnabled) {
  document.addEventListener('leavepictureinpicture', () => {
    const btn = document.getElementById('btn-pip');
    if (btn) { btn.textContent = '📺 PiP'; btn.classList.remove('active'); }
  });
}

// Background update check: every 6 hours, ping /api/check-update.
// If a newer version is on GitHub, show a small green dot on the Update
// button so the user knows without opening the modal. The modal is
// closed most of the time, so this avoids surprise-update prompts.
async function backgroundUpdateCheck() {
  try {
    const resp = await fetch('/api/check-update', { cache: 'no-store' });
    if (!resp.ok) return;
    const data = await resp.json();
    if (data && data.available && data.download_url) {
      const dot = document.getElementById('update-dot');
      if (dot) dot.style.display = 'inline-block';
      _latestUpdateInfo = data;
    }
  } catch (e) { /* offline, ignore */ }
}
setTimeout(backgroundUpdateCheck, 30000);     // first check 30s after page load
setInterval(backgroundUpdateCheck, 6 * 60 * 60 * 1000);  // then every 6 hours

// ── Scroll-to-top button & page navigation ──
(function() {
  const grid = document.getElementById('cctv-grid');
  const btn = document.createElement('button');
  btn.id = 'scroll-top-btn';
  btn.title = 'Scroll to top (or press Home)';
  btn.innerHTML = '↑';
  btn.onclick = function() {
    if (grid) grid.scrollTo({ top: 0, behavior: 'smooth' });
    else window.scrollTo({ top: 0, behavior: 'smooth' });
  };
  document.body.appendChild(btn);

  function checkScroll() {
    const scrollable = grid || document.documentElement;
    const scrollTop = grid ? grid.scrollTop : window.scrollY;
    const scrollHeight = grid ? grid.scrollHeight : document.documentElement.scrollHeight;
    const clientHeight = grid ? grid.clientHeight : window.innerHeight;
    const max = scrollHeight - clientHeight;
    if (scrollTop > 200 && max > 0) {
      btn.classList.add('visible');
    } else {
      btn.classList.remove('visible');
    }
  }

  if (grid) grid.addEventListener('scroll', checkScroll);
  window.addEventListener('scroll', checkScroll);
  setInterval(checkScroll, 500);

  // Keyboard navigation
  document.addEventListener('keydown', function(e) {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA' || e.target.tagName === 'SELECT') return;
    const target = grid || document.documentElement;
    const step = 100;
    if (e.key === 'PageDown') { e.preventDefault(); target.scrollTop += step * 5; }
    else if (e.key === 'PageUp') { e.preventDefault(); target.scrollTop -= step * 5; }
    else if (e.key === 'Home') { e.preventDefault(); target.scrollTo({ top: 0, behavior: 'smooth' }); }
    else if (e.key === 'End') { e.preventDefault(); target.scrollTo({ top: target.scrollHeight, behavior: 'smooth' }); }
    // ESC returns to grid view from single-view (Focus mode)
    else if (e.key === 'Escape' && viewMode === 'single') { e.preventDefault(); setViewMode('grid'); }
  });
})();
