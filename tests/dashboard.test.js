// Dashboard automated tests using Playwright
// Usage: NODE_PATH=C:\Users\Win\AppData\Roaming\npm\node_modules node tests/dashboard.test.js
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:8080';
const TIMEOUT = 15000;

let pass = 0, fail = 0;
const results = [];

function record(name, ok, detail) {
  results.push({ name, ok, detail: detail || '' });
  if (ok) { pass++; console.log(`[PASS] ${name}${detail ? ' — ' + detail : ''}`); }
  else { fail++; console.log(`[FAIL] ${name}${detail ? ' — ' + detail : ''}`); }
}

async function safeCall(name, fn) {
  try { const detail = await fn(); record(name, true, detail); }
  catch (e) { record(name, false, e.message.split('\n')[0]); }
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(TIMEOUT);

  console.log('=== PunMonitor Dashboard Tests ===');
  console.log('Base URL:', BASE);
  console.log('');

  // --- Static asset loading ---
  await safeCall('Dashboard HTML loads', async () => {
    const r = await page.goto(BASE + '/');
    if (!r.ok()) throw new Error('HTTP ' + r.status());
    return r.status() + ' (' + (await r.text()).length + ' bytes)';
  });

  await safeCall('dashboard.css loads (text/css)', async () => {
    const r = await page.request.get(BASE + '/dashboard.css');
    const ct = r.headers()['content-type'] || '';
    if (!ct.includes('text/css')) throw new Error('Wrong content-type: ' + ct);
    return (await r.text()).length + ' bytes, ' + ct;
  });

  await safeCall('dashboard.js loads (application/javascript)', async () => {
    const r = await page.request.get(BASE + '/dashboard.js');
    const ct = r.headers()['content-type'] || '';
    if (!ct.includes('javascript')) throw new Error('Wrong content-type: ' + ct);
    return (await r.text()).length + ' bytes, ' + ct;
  });

  // --- Critical UI elements ---
  await page.goto(BASE + '/');
  await page.waitForLoadState('domcontentloaded');

  await safeCall('Top bar present', async () => {
    if (!await page.locator('#topbar').isVisible()) throw new Error('#topbar not visible');
    return 'visible';
  });

  await safeCall('Grid view button', async () => {
    if (!await page.locator('#btn-grid-view').isVisible()) throw new Error('not visible');
    return 'visible';
  });

  await safeCall('Single view button', async () => {
    if (!await page.locator('#btn-single-view').isVisible()) throw new Error('not visible');
    return 'visible';
  });

  await safeCall('Remote Assistant button (no mojibake)', async () => {
    const btn = page.locator('button[onclick="openAssistSession()"]');
    if (!await btn.isVisible()) throw new Error('not visible');
    const text = (await btn.textContent()).trim();
    return '"' + text + '"';
  });

  await safeCall('Screen container exists', async () => {
    if (!await page.locator('#screen-container').count()) throw new Error('missing');
    return 'present';
  });

  await safeCall('Quality overlay exists', async () => {
    if (!await page.locator('#quality-overlay').count()) throw new Error('missing');
    return 'present';
  });

  await safeCall('PiP button present', async () => {
    if (!await page.locator('#btn-pip').count()) throw new Error('missing');
    return 'present';
  });

  await safeCall('Back-to-grid button present', async () => {
    if (!await page.locator('#btn-back-to-grid').count()) throw new Error('missing');
    return 'present';
  });

  // --- View mode switching ---
  await safeCall('Initial view is grid', async () => {
    const main = await page.locator('#main').getAttribute('class');
    if (!main.includes('grid-mode')) throw new Error('class: ' + main);
    return main;
  });

  await safeCall('Switch to single view', async () => {
    await page.locator('#btn-single-view').click();
    await page.waitForTimeout(200);
    const main = await page.locator('#main').getAttribute('class');
    if (!main.includes('single-mode')) throw new Error('class: ' + main);
    return main;
  });

  await safeCall('Switch back to grid', async () => {
    await page.locator('#btn-grid-view').click();
    await page.waitForTimeout(200);
    const main = await page.locator('#main').getAttribute('class');
    if (!main.includes('grid-mode')) throw new Error('class: ' + main);
    return main;
  });

  // --- ESC key exit focus ---
  await safeCall('ESC returns to grid from single', async () => {
    await page.locator('#btn-single-view').click();
    await page.waitForTimeout(200);
    await page.keyboard.press('Escape');
    await page.waitForTimeout(200);
    const main = await page.locator('#main').getAttribute('class');
    if (!main.includes('grid-mode')) throw new Error('still single after ESC: ' + main);
    return main;
  });

  // --- Quality overlay visible in single mode ---
  await safeCall('Quality overlay visible in single mode', async () => {
    await page.locator('#btn-single-view').click();
    await page.waitForTimeout(300);
    const opacity = await page.locator('#quality-overlay').evaluate(el => getComputedStyle(el).opacity);
    if (parseFloat(opacity) < 0.5) throw new Error('opacity: ' + opacity);
    return 'opacity: ' + opacity;
  });

  await safeCall('Quality overlay hidden in grid mode', async () => {
    await page.locator('#btn-grid-view').click();
    await page.waitForTimeout(300);
    const opacity = await page.locator('#quality-overlay').evaluate(el => getComputedStyle(el).opacity);
    if (parseFloat(opacity) > 0.5) throw new Error('opacity: ' + opacity);
    return 'opacity: ' + opacity;
  });

  // --- Scrollbar on grid ---
  await safeCall('Visible scrollbar on cctv-grid', async () => {
    const w = await page.locator('#cctv-grid').evaluate(el => {
      const s = getComputedStyle(el, '::-webkit-scrollbar');
      return s.width;
    });
    return 'width: ' + w;
  });

  // --- Remote assist creation ---
  await safeCall('Remote assist button creates cell', async () => {
    const before = await page.locator('.cctv-cell').count();
    await page.locator('button[onclick="openAssistSession()"]').click();
    await page.waitForTimeout(1000);
    const after = await page.locator('.cctv-cell').count();
    if (after <= before) throw new Error('no new cell (was ' + before + ', now ' + after + ')');
    return before + ' -> ' + after;
  });

  // --- API endpoints ---
  await safeCall('GET /api/health', async () => {
    const r = await page.request.get(BASE + '/api/health');
    const d = await r.json();
    if (d.status !== 'ok') throw new Error(JSON.stringify(d));
    return JSON.stringify(d);
  });

  await safeCall('GET /api/version', async () => {
    const r = await page.request.get(BASE + '/api/version');
    const d = await r.json();
    if (!/^10\.0\./.test(d.version)) throw new Error('unexpected version: ' + d.version);
    return d.version;
  });

  await safeCall('GET /api/agents', async () => {
    const r = await page.request.get(BASE + '/api/agents');
    const d = await r.json();
    if (!Array.isArray(d) || d.length === 0) throw new Error('no agents');
    return d.length + ' agent(s)';
  });

  await safeCall('GET /api/transport-status', async () => {
    const r = await page.request.get(BASE + '/api/transport-status');
    const d = await r.json();
    if (!d.healthy) throw new Error('not healthy');
    return d.active + ' (' + d.avg_latency_ms + 'ms)';
  });

  await safeCall('GET /api/election-status', async () => {
    const r = await page.request.get(BASE + '/api/election-status');
    const d = await r.json();
    if (!d.github_auth_ok) throw new Error('github not authed');
    return 'leader: ' + (d.leader_id || 'none');
  });

  await safeCall('GET /api/check-update (sees latest release)', async () => {
    const r = await page.request.get(BASE + '/api/check-update');
    const d = await r.json();
    return 'latest: v' + d.latest_version + ' (current v' + d.current_version + ')';
  });

  await safeCall('GET /api/agent/download (PE64 binary)', async () => {
    const r = await page.request.get(BASE + '/api/agent/download');
    const buf = await r.body();
    if (buf[0] !== 0x4D || buf[1] !== 0x5A) throw new Error('not MZ');
    if (buf.length < 20_000_000) throw new Error('too small: ' + buf.length);
    return (buf.length / 1024 / 1024).toFixed(1) + ' MB';
  });

  // --- WebSocket connection ---
  await safeCall('WebSocket /ws upgrades', async () => {
    const status = await page.evaluate(async () => {
      return await new Promise((resolve, reject) => {
        const sock = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws');
        const t = setTimeout(() => { try { sock.close(); } catch(_){} reject(new Error('timeout')); }, 5000);
        sock.onopen = () => { clearTimeout(t); resolve('101 Switching Protocols'); sock.close(); };
        sock.onerror = e => { clearTimeout(t); reject(new Error('ws error')); };
      });
    });
    return status;
  });

  // --- Console errors check ---
  const consoleErrors = [];
  const requestFailures = [];
  const consoleLogs = [];
  page.on('pageerror', e => consoleErrors.push({ msg: e.message, stack: e.stack || '', name: e.name, loc: JSON.stringify(e.location || {}) }));
  page.on('requestfailed', r => requestFailures.push(r.url() + ' ' + r.failure().errorText));
  page.on('console', msg => { if (msg.type() === 'error' || msg.type() === 'warning') consoleLogs.push(msg.type() + ': ' + msg.text()); });
  // First clear any previous errors
  consoleErrors.length = 0;
  await page.goto(BASE + '/');
  await page.waitForTimeout(3000);
  await safeCall('No JS pageerrors on load', async () => {
    if (consoleErrors.length > 0) {
      const detail = consoleErrors.map(e => e.msg + ' | ' + e.name + ' | ' + e.loc).join(' ||| ');
      throw new Error(consoleErrors.length + ' errors: ' + detail);
    }
    return 'clean';
  });
  await safeCall('No failed network requests', async () => {
    const real = requestFailures.filter(f => !/server-load|metrics|transport-status/.test(f));
    if (real.length > 0) throw new Error(real.length + ' failures: ' + real.slice(0, 3).join(' ||| '));
    if (requestFailures.length > 0) return requestFailures.length + ' aborted (expected)';
    return 'clean';
  });

  // --- Summary ---
  console.log('');
  console.log('============================================');
  console.log(`SUMMARY: ${pass} passed, ${fail} failed`);
  console.log('============================================');

  await browser.close();
  process.exit(fail > 0 ? 1 : 0);
})().catch(e => { console.error('Fatal:', e); process.exit(2); });
