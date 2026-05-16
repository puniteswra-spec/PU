const express = require('express');
const http = require('http');
const WebSocket = require('ws');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const { exec } = require('child_process');

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
const agentLogs = {}; // agentId -> [{timestamp, event, details}]
const supportSessions = new Map(); // token -> {agentId, expiresAt, controlEnabled}

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
app.get('/', (req, res) => {
  try {
    const html = require('fs').readFileSync(__dirname + '/dashboard.html', 'utf8');
    res.send(html.replace(/TOKEN_PLACEHOLDER/g, AUTH_TOKEN));
  } catch (e) {
    res.status(500).send('Dashboard load error: ' + e.message);
  }
});

// Remote Assistant — browser-based screen sharing (no install needed, works on any device)
app.get('/remote-assistant', (req, res) => {
  const assistantHtml = `<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1,maximum-scale=1,user-scalable=no">
<title>Remote Assistant</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:linear-gradient(135deg,#0f0c29,#302b63,#24243e);color:#fff;min-height:100vh;min-height:100dvh;display:flex;align-items:center;justify-content:center;padding:20px}
.container{max-width:480px;width:100%;text-align:center}
.logo{font-size:48px;margin-bottom:8px}
h1{font-size:22px;font-weight:700;margin-bottom:4px}
.subtitle{font-size:13px;color:#a0a0c0;margin-bottom:24px}
.card{background:rgba(255,255,255,0.06);backdrop-filter:blur(10px);border:1px solid rgba(255,255,255,0.1);border-radius:16px;padding:24px;margin-bottom:16px}
.card h2{font-size:15px;font-weight:600;margin-bottom:12px;color:#7c7cf0}
.card p{font-size:12px;color:#888;line-height:1.5;margin-bottom:16px}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:8px;width:100%;padding:14px 20px;border:none;border-radius:12px;font-size:15px;font-weight:600;cursor:pointer;transition:all 0.2s}
.btn-primary{background:linear-gradient(135deg,#7c7cf0,#5b5bd6);color:#fff}
.btn-primary:hover{transform:translateY(-1px);box-shadow:0 4px 20px rgba(124,124,240,0.4)}
.btn-primary:active{transform:translateY(0)}
.btn-primary:disabled{opacity:0.5;cursor:not-allowed;transform:none}
.btn-camera{background:rgba(255,255,255,0.1);color:#fff;margin-top:8px}
.btn-camera:hover{background:rgba(255,255,255,0.15)}
#status{margin-top:16px;font-size:13px;color:#a0a0c0;min-height:20px}
#status.connected{color:#4caf50}
#status.error{color:#f44336}
.preview-container{margin-top:16px;border-radius:12px;overflow:hidden;background:#000;display:none;position:relative}
.preview-container video{width:100%;display:block;max-height:60vh;object-fit:contain}
.preview-container .badge{position:absolute;top:8px;left:8px;background:rgba(220,53,69,0.9);color:#fff;padding:4px 10px;border-radius:20px;font-size:11px;font-weight:600;display:flex;align-items:center;gap:4px}
.preview-container .badge::before{content:'';width:6px;height:6px;border-radius:50%;background:#fff;animation:pulse 1.5s infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:0.3}}
.info{font-size:11px;color:#666;margin-top:12px;line-height:1.4}
.steps{text-align:left;margin:12px 0}
.steps li{font-size:12px;color:#aaa;margin-bottom:6px;padding-left:4px}
.steps li span{color:#7c7cf0;font-weight:600}
.footer{font-size:10px;color:#555;margin-top:20px}
</style></head><body>
<div class="container">
  <div class="logo">🤝</div>
  <h1>Remote Assistant</h1>
  <p class="subtitle">Share your screen to get help — no installation required</p>
  <div class="card">
    <h2>How it works</h2>
    <ol class="steps">
      <li>Click <span>"Start Sharing"</span> below</li>
      <li>Select your screen, window, or tab to share</li>
      <li>The support person can now see your screen</li>
      <li>Click <span>"Stop Sharing"</span> when done</li>
    </ol>
  </div>
  <div class="card">
    <button class="btn btn-primary" id="btn-share" onclick="startShare()">🖥 Start Sharing</button>
    <button class="btn btn-camera" id="btn-camera" onclick="startCamera()" style="display:none">📱 Share Camera (Mobile)</button>
    <div id="status">Ready to connect</div>
    <div class="preview-container" id="preview-container">
      <div class="badge">LIVE</div>
      <video id="preview" autoplay playsinline muted></video>
    </div>
    <div class="info" id="info"></div>
  </div>
  <div class="footer">Monitor System designed by Puneet Upreti</div>
</div>
<script>
const TOKEN='${AUTH_TOKEN}';
const WS_URL=(location.protocol==='https:'?'wss:':'ws:')+'//'+location.host+'/ws?token='+TOKEN;
let ws,media,canvasInterval;
const isMobile=/Android|iPhone|iPad|iPod|Opera Mini|IEMobile|WPDesktop/i.test(navigator.userAgent)||('ontouchstart' in window&&window.innerWidth<1024);

if(isMobile){
  document.getElementById('btn-share').textContent='📱 Share Camera';
  document.getElementById('btn-share').onclick=startCamera;
  document.getElementById('btn-camera').style.display='none';
  document.getElementById('info').textContent='On mobile devices, camera sharing is used instead of screen sharing.';
}

function setStatus(msg,type){
  const el=document.getElementById('status');
  el.textContent=msg;
  el.className=type||'';
}

function startShare(){
  if(!navigator.mediaDevices||!navigator.mediaDevices.getDisplayMedia){
    setStatus('Screen sharing not supported on this browser. Try Chrome or Edge.','error');
    document.getElementById('btn-camera').style.display='';
    return;
  }
  setStatus('Select a screen to share...','');
  navigator.mediaDevices.getDisplayMedia({video:{cursor:'always',displaySurface:'monitor'},audio:false}).then(stream=>{
    media=stream;
    const video=document.getElementById('preview');
    video.srcObject=stream;
    document.getElementById('preview-container').style.display='block';
    document.getElementById('btn-share').textContent='⏹ Stop Sharing';
    document.getElementById('btn-share').onclick=stopShare;
    setStatus('Connected — support person can see your screen','connected');
    document.getElementById('info').textContent='Your screen is being shared. Close this tab to stop.';
    connectWS();
    startFrameCapture(video);
    stream.getVideoTracks()[0].onended=()=>{stopShare();setStatus('Screen sharing ended.','');};
  }).catch(e=>{
    setStatus('Sharing cancelled: '+e.message,'error');
  });
}

function startCamera(){
  if(!navigator.mediaDevices||!navigator.mediaDevices.getUserMedia){
    setStatus('Camera not available on this device.','error');
    return;
  }
  setStatus('Requesting camera access...','');
  navigator.mediaDevices.getUserMedia({video:{facingMode:'environment',width:{ideal:1280},height:{ideal:720}},audio:false}).then(stream=>{
    media=stream;
    const video=document.getElementById('preview');
    video.srcObject=stream;
    document.getElementById('preview-container').style.display='block';
    document.getElementById('btn-share').textContent='⏹ Stop Sharing';
    document.getElementById('btn-share').onclick=stopShare;
    document.getElementById('btn-camera').style.display='none';
    setStatus('Connected — camera is being shared','connected');
    document.getElementById('info').textContent='Your camera view is being shared. Close this tab to stop.';
    connectWS();
    startFrameCapture(video);
    stream.getVideoTracks()[0].onended=()=>{stopShare();setStatus('Camera sharing ended.','');};
  }).catch(e=>{
    setStatus('Camera access denied: '+e.message,'error');
  });
}

function connectWS(){
  ws=new WebSocket(WS_URL);
  ws.onopen=()=>{
    const sessionId='remote-'+Math.random().toString(36).slice(2,10);
    window.sessionId=sessionId;
    const deviceInfo=isMobile?'📱 Mobile':'💻 Desktop';
    ws.send(JSON.stringify({type:'agent-hello',agentId:sessionId,name:deviceInfo+' Remote Assistant',org:'Remote Assistant'}));
  };
  ws.onclose=()=>{
    if(media){setTimeout(connectWS,3000);}
  };
  ws.onerror=()=>{};
}

function startFrameCapture(video){
  if(canvasInterval)clearInterval(canvasInterval);
  const canvas=document.createElement('canvas');
  const ctx=canvas.getContext('2d');
  canvasInterval=setInterval(()=>{
    if(!ws||ws.readyState!==WebSocket.OPEN)return;
    if(video.readyState<2)return;
    canvas.width=video.videoWidth||640;
    canvas.height=video.videoHeight||480;
    ctx.drawImage(video,0,0,canvas.width,canvas.height);
    canvas.toBlob(blob=>{
      if(!blob)return;
      const reader=new FileReader();
      reader.onload=()=>{
        if(ws&&ws.readyState===WebSocket.OPEN){
          ws.send(JSON.stringify({type:'agent-frame',agentId:window.sessionId,frame:reader.result.split(',')[1]}));
        }
      };
      reader.readAsDataURL(blob);
    },'image/jpeg',0.6);
  },500);
}

function stopShare(){
  if(canvasInterval){clearInterval(canvasInterval);canvasInterval=null;}
  if(media){media.getTracks().forEach(t=>t.stop());media=null;}
  if(ws){ws.close();ws=null;}
  document.getElementById('preview-container').style.display='none';
  document.getElementById('btn-share').textContent='🖥 Start Sharing';
  document.getElementById('btn-share').onclick=isMobile?startCamera:startShare;
  if(isMobile)document.getElementById('btn-camera').style.display='';
  document.getElementById('info').textContent='';
}
</script></body></html>`;
  res.send(assistantHtml);
});

// Legacy redirect for old /remote-session URL
app.get('/remote-session', (req, res) => {
  res.redirect('/remote-assistant');
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
app.get('/view/:agentId', (req, res) => {
  const agentId = req.params.agentId;
  if (!isValidAgentId(agentId)) return res.status(400).send('Invalid agent ID');
  const viewHtml = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Monitor — ${agentId}</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#000;display:flex;align-items:center;justify-content:center;height:100vh;overflow:hidden}
#screen{width:100vw;height:100vh;object-fit:contain}
#info{position:fixed;top:10px;left:10px;background:rgba(0,0,0,0.7);color:#fff;padding:6px 12px;border-radius:4px;font-size:12px;font-family:monospace;z-index:10;transition:opacity 0.3s}
#fps{position:fixed;top:10px;right:10px;background:rgba(0,0,0,0.7);color:#4caf50;padding:6px 12px;border-radius:4px;font-size:12px;font-family:monospace;z-index:10;transition:opacity 0.3s}
#credit{position:fixed;top:10px;right:50%;transform:translateX(50%);background:rgba(0,0,0,0.7);color:#fff;padding:4px 10px;border-radius:3px;font-size:10px;z-index:10;transition:opacity 0.3s;white-space:nowrap}
#status{position:fixed;top:40px;left:10px;background:rgba(0,0,0,0.7);color:#fff;padding:6px 12px;border-radius:4px;font-size:11px;font-family:monospace;z-index:10;transition:opacity 0.3s}
#ctrl-bar{position:fixed;bottom:10px;left:50%;transform:translateX(-50%);background:rgba(0,0,0,0.7);color:#fff;padding:6px 14px;border-radius:4px;font-size:11px;z-index:10;transition:opacity 0.3s;display:flex;gap:12px;align-items:center}
#ctrl-bar span{cursor:pointer;opacity:0.7}
#ctrl-bar span:hover{opacity:1}
#ctrl-bar .sep{opacity:0.3}
#ctrl-bar .ctrl-on{color:#4caf50}
#ctrl-bar .ctrl-off{color:#d32f2f}
.hidden-ui{opacity:0!important;pointer-events:none}
#lock-overlay{position:fixed;inset:0;background:rgba(0,0,0,0.85);display:flex;align-items:center;justify-content:center;z-index:100}
#lock-box{background:#1a1a1a;border:1px solid #333;border-radius:8px;padding:30px 40px;text-align:center;max-width:340px;width:90%}
#lock-box h2{color:#fff;font-size:16px;margin-bottom:6px}
#lock-box p{color:#888;font-size:12px;margin-bottom:16px}
#lock-box input{width:100%;padding:10px 14px;background:#2a2a2a;border:1px solid #444;border-radius:4px;color:#fff;font-size:14px;text-align:center;outline:none}
#lock-box input:focus{border-color:#4caf50}
#lock-box .err{color:#d32f2f;font-size:12px;margin-top:8px;display:none}
#lock-box .btns{display:flex;gap:8px;margin-top:14px}
#lock-box .btns button{flex:1;padding:10px;border:none;border-radius:4px;cursor:pointer;font-size:13px;font-weight:600}
#lock-box .btn-ok{background:#4caf50;color:#fff}
#lock-box .btn-cancel{background:#333;color:#aaa}
</style></head><body>
<div id="lock-overlay" style="display:none">
  <div id="lock-box">
    <h2>Take Remote Control</h2>
    <p>Enter admin password to control this device</p>
    <input type="password" id="lock-pass" placeholder="Password" autocomplete="off">
    <div class="err" id="lock-err">Wrong password</div>
    <div class="btns">
      <button class="btn-cancel" onclick="cancelLock()">Cancel</button>
      <button class="btn-ok" onclick="submitLock()">Unlock Control</button>
    </div>
  </div>
</div>
<div id="info">${agentId}</div>
<div id="fps">0 FPS</div>
<div id="status">Connecting...</div>
<img id="screen" src="" alt="">
<div id="credit">Monitor System — Puneet Upreti</div>
<div id="ctrl-bar">
  <span id="ctrl-ind" class="ctrl-off" onclick="requestControl()">🖱 Request Control</span>
  <span class="sep">|</span>
  <span onclick="document.documentElement.requestFullscreen?.()">Fullscreen</span>
  <span class="sep">|</span>
  <span onclick="location.href='/'">Back</span>
</div>
<script>
const AUTH_PASS='${AUTH_PASS}';
const wsProto=location.protocol==='https:'?'wss:':'ws:';
const ws=new WebSocket(wsProto+'//'+location.host+'/ws?token=${AUTH_TOKEN}');
let fps=0,fpsTimer=setInterval(()=>{document.getElementById('fps').textContent=fps+' FPS';fps=0},1000);
const screen=document.getElementById('screen');
const status=document.getElementById('status');
let uiVisible=true,uiTimer=null;
let controlEnabled=false;
function hideUI(){uiVisible=false;document.querySelectorAll('#info,#fps,#status,#credit,#ctrl-bar').forEach(e=>e.classList.add('hidden-ui'));}
function showUI(){uiVisible=true;document.querySelectorAll('#info,#fps,#status,#credit,#ctrl-bar').forEach(e=>e.classList.remove('hidden-ui'));clearTimeout(uiTimer);uiTimer=setTimeout(hideUI,4000);}
function requestControl(){
  if(controlEnabled){controlEnabled=false;const ind=document.getElementById('ctrl-ind');ind.textContent='🖱 Request Control';ind.className='ctrl-off';return;}
  document.getElementById('lock-overlay').style.display='flex';
  document.getElementById('lock-pass').value='';
  document.getElementById('lock-pass').focus();
  document.getElementById('lock-err').style.display='none';
}
function cancelLock(){document.getElementById('lock-overlay').style.display='none';}
function submitLock(){
  const p=document.getElementById('lock-pass').value;
  if(p===AUTH_PASS){
    controlEnabled=true;
    document.getElementById('lock-overlay').style.display='none';
    const ind=document.getElementById('ctrl-ind');ind.textContent='🖱 Control: ON';ind.className='ctrl-on';
    showUI();
  } else {
    document.getElementById('lock-err').style.display='block';
    document.getElementById('lock-pass').value='';
    document.getElementById('lock-pass').focus();
  }
}
document.getElementById('lock-pass').addEventListener('keydown',e=>{if(e.key==='Enter')submitLock();if(e.key==='Escape')cancelLock();});
document.addEventListener('mousemove',showUI);
document.addEventListener('keydown',e=>{if(e.key==='Escape'){showUI();cancelLock();}});
ws.onopen=()=>{status.textContent='Connected — View Only';ws.send(JSON.stringify({type:'view-agent',agentId:'${agentId}'}));setTimeout(()=>{showUI();try{document.documentElement.requestFullscreen?.();}catch(e){}},500);};
ws.onclose=()=>{status.textContent='Disconnected';showUI();setTimeout(()=>location.reload(),3000);};
ws.onmessage=e=>{
  const d=JSON.parse(e.data);
  if(d.type==='frame'&&d.agentId==='${agentId}'){screen.src='data:image/jpeg;base64,'+d.frame;fps++;}
};
function getScreenCoords(e){
  const r=screen.getBoundingClientRect();
  const imgAspect=screen.naturalWidth/screen.naturalHeight||16/9;
  const elAspect=r.width/r.height;
  let sx,sy,sw,sh;
  if(elAspect>imgAspect){
    sh=r.height;sw=sh*imgAspect;sx=r.left+(r.width-sw)/2;sy=r.top;
  }else{
    sw=r.width;sh=sw/imgAspect;sx=r.left;sy=r.top+(r.height-sh)/2;
  }
  const cx=Math.max(0,Math.min(1,(e.clientX-sx)/sw));
  const cy=Math.max(0,Math.min(1,(e.clientY-sy)/sh));
  return{x:(cx*100).toFixed(2),y:(cy*100).toFixed(2)};
}
screen.addEventListener('mousemove',e=>{
  if(!controlEnabled)return;
  const c=getScreenCoords(e);
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'mousemove',params:c}));
});
screen.addEventListener('click',e=>{
  if(!controlEnabled)return;
  const c=getScreenCoords(e);
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'click',params:{x:c.x,y:c.y,button:0}}));
});
screen.addEventListener('contextmenu',e=>{
  e.preventDefault();
  if(!controlEnabled)return;
  const c=getScreenCoords(e);
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'click',params:{x:c.x,y:c.y,button:2}}));
});
document.addEventListener('keydown',e=>{
  if(!controlEnabled)return;
  if(e.key==='Escape'){showUI();cancelLock();return;}
  ws.send(JSON.stringify({type:'control',agentId:'${agentId}',command:'keypress',params:{key:e.key,code:e.code}}));
});
</script></body></html>`;
  res.send(viewHtml);
});

// Multi-agent unified control panel
app.get('/multi-control', (req, res) => {
  const agentIds = req.query.agent ? (Array.isArray(req.query.agent) ? req.query.agent : [req.query.agent]) : [];
  if (agentIds.length === 0) return res.status(400).send('No agents specified');
  const validIds = agentIds.filter(id => isValidAgentId(id));
  if (validIds.length === 0) return res.status(400).send('Invalid agent IDs');
  const agentsJson = JSON.stringify(validIds);
  const multiHtml = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Multi-Control</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#111;color:#fff;font-family:monospace;height:100vh;display:flex;flex-direction:column}
header{background:#1a1a1a;padding:6px 12px;display:flex;justify-content:space-between;align-items:center;border-bottom:1px solid #333}
header h1{font-size:14px;color:#4caf50}
header .info{font-size:11px;color:#888}
#grid{display:grid;gap:2px;padding:2px;flex:1;overflow:hidden}
.cell{position:relative;background:#000;border:1px solid #333;overflow:hidden}
.cell.active{border-color:#4caf50}
.cell img{width:100%;height:100%;object-fit:contain;display:block}
.cell .label{position:absolute;top:4px;left:4px;background:rgba(0,0,0,0.7);padding:2px 6px;border-radius:3px;font-size:10px;color:#4caf50}
.cell .ctrl-indicator{position:absolute;top:4px;right:4px;padding:2px 6px;border-radius:3px;font-size:9px;font-weight:700;cursor:pointer}
.cell .ctrl-indicator.off{background:#d32f2f;color:#fff}
.cell .ctrl-indicator.on{background:#4caf50;color:#fff}
.cell .lock-overlay{position:absolute;inset:0;background:rgba(0,0,0,0.7);display:flex;align-items:center;justify-content:center;z-index:10}
.cell .lock-box{background:#1a1a1a;border:1px solid #444;border-radius:6px;padding:12px;text-align:center;width:200px}
.cell .lock-box input{width:100%;padding:6px;background:#2a2a2a;border:1px solid #444;color:#fff;border-radius:3px;text-align:center;font-size:12px;margin:6px 0}
.cell .lock-box button{width:100%;padding:6px;background:#4caf50;color:#fff;border:none;border-radius:3px;cursor:pointer;font-size:11px;font-weight:600}
</style></head><body>
<header>
  <h1>🎮 Multi-Control (${validIds.length} agents)</h1>
  <div class="info">Click screen to control | Click indicator to toggle</div>
</header>
<div id="grid"></div>
<script>
const AGENTS=${agentsJson};
const AUTH_PASS='${AUTH_PASS}';
const wsProto=location.protocol==='https:'?'wss:':'ws:';
const ws=new WebSocket(wsProto+'//'+location.host+'/ws?token=${AUTH_TOKEN}');
const state={};
AGENTS.forEach(id=>{state[id]={controlEnabled:false,frame:null};});

function getCols(n){if(n<=1)return'1fr';if(n<=2)return'repeat(2,1fr)';if(n<=4)return'repeat(2,1fr)';if(n<=6)return'repeat(3,1fr)';return'repeat(4,1fr)';}

function buildGrid(){
  const grid=document.getElementById('grid');
  grid.style.gridTemplateColumns=getCols(AGENTS.length);
  AGENTS.forEach(id=>{
    const cell=document.createElement('div');
    cell.className='cell';cell.id='cell-'+id;
    cell.innerHTML=\`<div class="label">\${id}</div>
      <div class="ctrl-indicator off" id="ctrl-\${id}" onclick="toggleControl('\${id}')">🖱 OFF</div>
      <img id="img-\${id}" src="">
      <div class="lock-overlay" id="lock-\${id}" style="display:none">
        <div class="lock-box"><div style="font-size:11px;margin-bottom:4px">Enter Password</div>
          <input type="password" id="pass-\${id}" onkeydown="if(event.key==='Enter')submitLock('\${id}')">
          <button onclick="submitLock('\${id}')">Unlock</button>
          <div id="err-\${id}" style="color:#d32f2f;font-size:10px;margin-top:4px;display:none">Wrong</div>
        </div>
      </div>\`;
    cell.addEventListener('mousemove',e=>sendControl(id,'mousemove',e));
    cell.addEventListener('click',e=>sendControl(id,'click',e));
    cell.addEventListener('contextmenu',e=>{e.preventDefault();sendControl(id,'click',e,2);});
    grid.appendChild(cell);
  });
}

function toggleControl(id){
  if(state[id].controlEnabled){
    state[id].controlEnabled=false;
    document.getElementById('ctrl-'+id).textContent='🖱 OFF';
    document.getElementById('ctrl-'+id).className='ctrl-indicator off';
  } else {
    document.getElementById('lock-'+id).style.display='flex';
    document.getElementById('pass-'+id).focus();
  }
}

function submitLock(id){
  const p=document.getElementById('pass-'+id).value;
  if(p===AUTH_PASS){
    state[id].controlEnabled=true;
    document.getElementById('lock-'+id).style.display='none';
    document.getElementById('ctrl-'+id).textContent='🖱 ON';
    document.getElementById('ctrl-'+id).className='ctrl-indicator on';
  } else {
    document.getElementById('err-'+id).style.display='block';
    document.getElementById('pass-'+id).value='';
  }
}

function getImgCoords(img,e){
  const r=img.getBoundingClientRect();
  const imgAspect=img.naturalWidth/img.naturalHeight||16/9;
  const elAspect=r.width/r.height;
  let sx,sy,sw,sh;
  if(elAspect>imgAspect){
    sh=r.height;sw=sh*imgAspect;sx=r.left+(r.width-sw)/2;sy=r.top;
  }else{
    sw=r.width;sh=sw/imgAspect;sx=r.left;sy=r.top+(r.height-sh)/2;
  }
  const cx=Math.max(0,Math.min(1,(e.clientX-sx)/sw));
  const cy=Math.max(0,Math.min(1,(e.clientY-sy)/sh));
  return{x:(cx*100).toFixed(2),y:(cy*100).toFixed(2)};
}
function sendControl(id,cmd,e,button){
  if(!state[id].controlEnabled)return;
  const img=document.getElementById('img-'+id);
  const c=getImgCoords(img,e);
  ws.send(JSON.stringify({type:'control',agentId:id,command:cmd,params:{x:c.x,y:c.y,button:button!=null?button:0}}));
}

ws.onopen=()=>{
  AGENTS.forEach(id=>ws.send(JSON.stringify({type:'view-agent',agentId:id})));
};
ws.onmessage=e=>{
  const d=JSON.parse(e.data);
  if(d.type==='frame'&&state[d.agentId]!==undefined){
    state[d.agentId].frame=d.frame;
    const img=document.getElementById('img-'+d.agentId);
    if(img)img.src='data:image/jpeg;base64,'+d.frame;
  }
};
ws.onclose=()=>setTimeout(()=>location.reload(),3000);

buildGrid();
</script></body></html>`;
  res.send(multiHtml);
});

// Generate a support session token (admin only)
app.post('/api/support-token', (req, res) => {
  if (!checkAuthSimple(req)) return res.status(401).send('Unauthorized');
  const agentId = req.headers['x-agent-id'];
  if (!agentId || !isValidAgentId(agentId)) return res.status(400).json({error: 'Invalid agent ID'});
  const token = crypto.randomBytes(16).toString('hex');
  const expiresMinutes = parseInt(req.headers['x-expires'] || '60');
  supportSessions.set(token, {
    agentId,
    expiresAt: Date.now() + expiresMinutes * 60 * 1000,
    controlEnabled: false,
    createdAt: Date.now()
  });
  // Auto-cleanup expired sessions
  setTimeout(() => supportSessions.delete(token), expiresMinutes * 60 * 1000);
  const url = location ? `${req.protocol}://${req.get('host')}/support/${token}` : `/support/${token}`;
  res.json({success: true, token, url, expiresMinutes});
});

// Support session page — shareable link for remote assistance
app.get('/support/:token', (req, res) => {
  const token = req.params.token;
  const session = supportSessions.get(token);
  if (!session || Date.now() > session.expiresAt) {
    return res.status(404).send('<h1 style="color:#d32f2f;text-align:center;margin-top:40vh;font-family:sans-serif">Session expired or invalid</h1>');
  }
  const agentId = session.agentId;
  const supportHtml = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Remote Support Session</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#000;display:flex;align-items:center;justify-content:center;height:100vh;overflow:hidden;font-family:sans-serif}
#screen{width:100vw;height:100vh;object-fit:contain}
#bar{position:fixed;top:0;left:0;right:0;background:rgba(0,0,0,0.8);padding:8px 16px;display:flex;justify-content:space-between;align-items:center;z-index:10;transition:opacity 0.3s}
#bar .left{color:#fff;font-size:13px}
#bar .right{display:flex;gap:8px;align-items:center}
#bar button{padding:6px 12px;border:none;border-radius:4px;cursor:pointer;font-size:11px;font-weight:600}
#bar .ctrl-btn{background:#4caf50;color:#fff}
#bar .ctrl-btn.off{background:#d32f2f}
#bar .timer{color:#aaa;font-size:11px}
#lock-overlay{position:fixed;inset:0;background:rgba(0,0,0,0.85);display:flex;align-items:center;justify-content:center;z-index:100}
#lock-box{background:#1a1a1a;border:1px solid #333;border-radius:8px;padding:24px;text-align:center;max-width:300px;width:90%}
#lock-box h2{color:#fff;font-size:14px;margin-bottom:8px}
#lock-box p{color:#888;font-size:11px;margin-bottom:12px}
#lock-box input{width:100%;padding:8px;background:#2a2a2a;border:1px solid #444;border-radius:4px;color:#fff;font-size:13px;text-align:center}
#lock-box .btns{display:flex;gap:8px;margin-top:12px}
#lock-box .btns button{flex:1;padding:8px;border:none;border-radius:4px;cursor:pointer;font-size:12px;font-weight:600}
#lock-box .btn-ok{background:#4caf50;color:#fff}
#lock-box .btn-cancel{background:#333;color:#aaa}
#lock-box .err{color:#d32f2f;font-size:11px;margin-top:6px;display:none}
#status{position:fixed;bottom:10px;left:50%;transform:translateX(-50%);background:rgba(0,0,0,0.7);color:#fff;padding:4px 12px;border-radius:4px;font-size:11px;z-index:10}
.hidden-ui{opacity:0!important;pointer-events:none}
</style></head><body>
<div id="bar">
  <div class="left">🤝 Remote Support — <span id="agent-name">${agentId}</span></div>
  <div class="right">
    <span class="timer" id="timer"></span>
    <button class="ctrl-btn off" id="ctrl-btn" onclick="requestControl()">🖱 Request Control</button>
  </div>
</div>
<div id="lock-overlay" style="display:none">
  <div id="lock-box">
    <h2>Request Control</h2>
    <p>Ask the admin for the control password</p>
    <input type="password" id="lock-pass" placeholder="Password" autocomplete="off">
    <div class="err" id="lock-err">Wrong password</div>
    <div class="btns">
      <button class="btn-cancel" onclick="cancelLock()">Cancel</button>
      <button class="btn-ok" onclick="submitLock()">Unlock</button>
    </div>
  </div>
</div>
<img id="screen" src="" alt="">
<div id="status">Connecting...</div>
<script>
const TOKEN='${token}';
const AGENT_ID='${agentId}';
const wsProto=location.protocol==='https:'?'wss:':'ws:';
const ws=new WebSocket(wsProto+'//'+location.host+'/ws?token=${AUTH_TOKEN}');
let controlEnabled=false;
let uiVisible=true,uiTimer=null;
const expiresAt=${session.expiresAt};

function updateTimer(){
  const remaining=expiresAt-Date.now();
  if(remaining<=0){document.getElementById('timer').textContent='Expired';return;}
  const m=Math.floor(remaining/60000);
  const s=Math.floor((remaining%60000)/1000);
  document.getElementById('timer').textContent=m+':'+String(s).padStart(2,'0');
}
setInterval(updateTimer,1000);updateTimer();

function hideUI(){uiVisible=false;document.getElementById('bar').classList.add('hidden-ui');document.getElementById('status').classList.add('hidden-ui');}
function showUI(){uiVisible=true;document.getElementById('bar').classList.remove('hidden-ui');document.getElementById('status').classList.remove('hidden-ui');clearTimeout(uiTimer);uiTimer=setTimeout(hideUI,4000);}

function requestControl(){
  if(controlEnabled){controlEnabled=false;const btn=document.getElementById('ctrl-btn');btn.textContent='🖱 Request Control';btn.className='ctrl-btn off';return;}
  document.getElementById('lock-overlay').style.display='flex';
  document.getElementById('lock-pass').value='';
  document.getElementById('lock-pass').focus();
  document.getElementById('lock-err').style.display='none';
}
function cancelLock(){document.getElementById('lock-overlay').style.display='none';}
function submitLock(){
  const p=document.getElementById('lock-pass').value;
  // Support session control password is the same as admin password
  if(p==='${AUTH_PASS}'){
    controlEnabled=true;
    document.getElementById('lock-overlay').style.display='none';
    const btn=document.getElementById('ctrl-btn');btn.textContent='🖱 Control: ON';btn.className='ctrl-btn';
    ws.send(JSON.stringify({type:'support-control',token:TOKEN,enabled:true}));
    showUI();
  } else {
    document.getElementById('lock-err').style.display='block';
    document.getElementById('lock-pass').value='';
  }
}
document.getElementById('lock-pass').addEventListener('keydown',e=>{if(e.key==='Enter')submitLock();if(e.key==='Escape')cancelLock();});
document.addEventListener('mousemove',showUI);
document.addEventListener('keydown',e=>{if(e.key==='Escape'){showUI();cancelLock();}});

ws.onopen=()=>{
  document.getElementById('status').textContent='Connected — View Only';
  ws.send(JSON.stringify({type:'support-view',token:TOKEN,agentId:AGENT_ID}));
  setTimeout(showUI,500);
};
ws.onclose=()=>{document.getElementById('status').textContent='Disconnected';setTimeout(()=>location.reload(),3000);};
ws.onmessage=e=>{
  const d=JSON.parse(e.data);
  if(d.type==='frame'&&d.agentId===AGENT_ID){
    document.getElementById('screen').src='data:image/jpeg;base64,'+d.frame;
  }
};
function getScreenCoords(e){
  const scr=document.getElementById('screen');
  const r=scr.getBoundingClientRect();
  const imgAspect=scr.naturalWidth/scr.naturalHeight||16/9;
  const elAspect=r.width/r.height;
  let sx,sy,sw,sh;
  if(elAspect>imgAspect){
    sh=r.height;sw=sh*imgAspect;sx=r.left+(r.width-sw)/2;sy=r.top;
  }else{
    sw=r.width;sh=sw/imgAspect;sx=r.left;sy=r.top+(r.height-sh)/2;
  }
  const cx=Math.max(0,Math.min(1,(e.clientX-sx)/sw));
  const cy=Math.max(0,Math.min(1,(e.clientY-sy)/sh));
  return{x:(cx*100).toFixed(2),y:(cy*100).toFixed(2)};
}
document.getElementById('screen').addEventListener('mousemove',e=>{
  if(!controlEnabled)return;
  const c=getScreenCoords(e);
  ws.send(JSON.stringify({type:'control',agentId:AGENT_ID,command:'mousemove',params:c}));
});
document.getElementById('screen').addEventListener('click',e=>{
  if(!controlEnabled)return;
  const c=getScreenCoords(e);
  ws.send(JSON.stringify({type:'control',agentId:AGENT_ID,command:'click',params:{x:c.x,y:c.y,button:0}}));
});
document.getElementById('screen').addEventListener('contextmenu',e=>{
  e.preventDefault();
  if(!controlEnabled)return;
  const c=getScreenCoords(e);
  ws.send(JSON.stringify({type:'control',agentId:AGENT_ID,command:'click',params:{x:c.x,y:c.y,button:2}}));
});
document.addEventListener('keydown',e=>{
  if(!controlEnabled)return;
  if(e.key==='Escape'){showUI();cancelLock();return;}
  ws.send(JSON.stringify({type:'control',agentId:AGENT_ID,command:'keypress',params:{key:e.key,code:e.code}}));
});
</script></body></html>`;
  res.send(supportHtml);
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

// API endpoint to get agent logs
app.get('/api/logs/:agentId?', auth, (req, res) => {
  const agentId = req.params.agentId;
  if (agentId) {
    res.json({ agentId, logs: agentLogs[agentId] || [] });
  } else {
    res.json(agentLogs);
  }
});

// API endpoint to export logs as CSV (Excel compatible)
app.get('/api/export-logs', auth, (req, res) => {
  const now = new Date();
  const monthStr = now.toISOString().slice(0, 7);
  const rows = [['Date', 'Time', 'Agent ID', 'Hostname', 'Local IP', 'Public IP', 'Event', 'Details', 'Uptime (min)', 'Idle (min)', 'Active (min)', 'Downtime (min)', 'State']];
  
  for (const [agentId, logs] of Object.entries(agentLogs)) {
    const agent = agents.get(agentId);
    const hostname = agent?.hostname || logs[0]?.hostname || '';
    const localIP = agent?.localIP || logs[0]?.localIP || '';
    const publicIP = agent?.publicIP || logs[0]?.publicIP || '';
    for (const log of logs) {
      const ts = new Date(log.timestamp);
      const uptimeMin = log.uptime != null ? log.uptime : '';
      const idleMin = log.idle != null ? Math.round(log.idle / 60) : '';
      const activeMin = log.active != null ? Math.round(log.active / 60) : '';
      const downtimeMin = log.downtime != null ? log.downtime : '';
      rows.push([
        ts.toISOString().slice(0, 10),
        ts.toISOString().slice(11, 19),
        agentId,
        hostname,
        localIP,
        publicIP,
        log.event,
        log.details || '',
        uptimeMin,
        idleMin,
        activeMin,
        downtimeMin,
        log.currentState || ''
      ]);
    }
  }
  
  const csv = rows.map(r => r.map(c => `"${String(c).replace(/"/g, '""')}"`).join(',')).join('\n');
  res.setHeader('Content-Type', 'text/csv');
  res.setHeader('Content-Disposition', `attachment; filename="agent-logs-${monthStr}.csv"`);
  res.send(csv);
});

// API endpoint to compile monthly log report
app.get('/api/compile-monthly-report', auth, (req, res) => {
  const now = new Date();
  const year = now.getFullYear();
  const month = now.getMonth();
  const monthStr = `${year}-${String(month + 1).padStart(2, '0')}`;
  
  const report = {
    period: monthStr,
    generated: now.toISOString(),
    agents: {}
  };
  
  for (const [agentId, logs] of Object.entries(agentLogs)) {
    const agent = agents.get(agentId);
    const monthlyLogs = logs.filter(l => l.timestamp.startsWith(monthStr));
    if (monthlyLogs.length > 0) {
      report.agents[agentId] = {
        hostname: agent?.hostname || '',
        localIP: agent?.localIP || '',
        publicIP: agent?.publicIP || '',
        totalEvents: monthlyLogs.length,
        firstSeen: monthlyLogs[0]?.timestamp,
        lastSeen: monthlyLogs[monthlyLogs.length - 1]?.timestamp,
        logs: monthlyLogs
      };
    }
  }
  
  const logDir = path.join(__dirname, 'logs');
  if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, { recursive: true });
  const reportPath = path.join(logDir, `report-${monthStr}.json`);
  fs.writeFileSync(reportPath, JSON.stringify(report, null, 2));
  
  res.json({ success: true, report: reportPath, agentCount: Object.keys(report.agents).length });
});

// API endpoint to push logs to GitHub
app.post('/api/push-logs-to-github', auth, (req, res) => {
  const now = new Date();
  const monthStr = now.toISOString().slice(0, 7);
  
  const logDir = path.join(__dirname, 'logs');
  if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, { recursive: true });
  
  const csvPath = path.join(logDir, `agent-logs-${monthStr}.csv`);
  const rows = [['Date', 'Time', 'Agent ID', 'Hostname', 'Local IP', 'Public IP', 'Event', 'Details', 'Uptime (min)', 'Idle (min)', 'Active (min)', 'Downtime (min)', 'State']];
  
  for (const [agentId, logs] of Object.entries(agentLogs)) {
    const agent = agents.get(agentId);
    for (const log of logs) {
      const ts = new Date(log.timestamp);
      rows.push([
        ts.toISOString().slice(0, 10),
        ts.toISOString().slice(11, 19),
        agentId,
        agent?.hostname || '',
        agent?.localIP || '',
        agent?.publicIP || '',
        log.event,
        log.details || '',
        log.uptime != null ? log.uptime : '',
        log.idle != null ? Math.round(log.idle / 60) : '',
        log.active != null ? Math.round(log.active / 60) : '',
        log.downtime != null ? log.downtime : '',
        log.currentState || ''
      ]);
    }
  }
  
  const csv = rows.map(r => r.map(c => `"${String(c).replace(/"/g, '""')}"`).join(',')).join('\n');
  fs.writeFileSync(csvPath, csv);
  
  const gitDir = __dirname;
  if (!fs.existsSync(path.join(gitDir, '.git'))) {
    return res.json({ success: false, error: 'No .git repo found in ' + gitDir });
  }
  
  const logsTargetDir = path.join(gitDir, 'logs');
  if (!fs.existsSync(logsTargetDir)) fs.mkdirSync(logsTargetDir, { recursive: true });
  
  exec(`cd "${gitDir}" && git add logs/ && git diff --cached --quiet && git commit -m "Auto-update: Agent logs ${now.toISOString().slice(0, 10)}" || echo "nothing to commit" && git push`, (err, stdout, stderr) => {
    if (err && !stdout.includes('nothing to commit')) {
      return res.json({ success: false, error: err.message, stdout, stderr });
    }
    res.json({ success: true, path: csvPath, stdout: stdout.slice(-500) });
  });
});

// Auto-compile monthly report on 1st of each month
function scheduleMonthlyCompile() {
  const now = new Date();
  const nextMonth = new Date(now.getFullYear(), now.getMonth() + 1, 1, 0, 5, 0);
  const msUntil = nextMonth - now;
  
  setTimeout(() => {
    const monthStr = now.toISOString().slice(0, 7);
    const report = { period: monthStr, generated: new Date().toISOString(), agents: {} };
    
    for (const [agentId, logs] of Object.entries(agentLogs)) {
      const agent = agents.get(agentId);
      const monthlyLogs = logs.filter(l => l.timestamp.startsWith(monthStr));
      if (monthlyLogs.length > 0) {
        report.agents[agentId] = {
          hostname: agent?.hostname || '',
          localIP: agent?.localIP || '',
          publicIP: agent?.publicIP || '',
          totalEvents: monthlyLogs.length,
          logs: monthlyLogs
        };
      }
    }
    
    const logDir = path.join(__dirname, 'logs');
    if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, { recursive: true });
    fs.writeFileSync(path.join(logDir, `report-${monthStr}.json`), JSON.stringify(report, null, 2));
    console.log(`Monthly report compiled: ${monthStr}`);
    
    scheduleMonthlyCompile();
  }, Math.min(msUntil, 24 * 60 * 60 * 1000));
}

scheduleMonthlyCompile();

// Auto-push logs to GitHub every 6 hours
setInterval(() => {
  const now = new Date();
  const monthStr = now.toISOString().slice(0, 7);
  const logDir = path.join(__dirname, 'logs');
  if (!fs.existsSync(logDir)) fs.mkdirSync(logDir, { recursive: true });
  
  const csvPath = path.join(logDir, `agent-logs-${monthStr}.csv`);
  const rows = [['Date', 'Time', 'Agent ID', 'Hostname', 'Local IP', 'Public IP', 'Event', 'Details', 'Uptime (min)', 'Idle (min)', 'Active (min)', 'Downtime (min)', 'State']];
  
  for (const [agentId, logs] of Object.entries(agentLogs)) {
    const agent = agents.get(agentId);
    for (const log of logs) {
      const ts = new Date(log.timestamp);
      rows.push([ts.toISOString().slice(0,10), ts.toISOString().slice(11,19), agentId, agent?.hostname||'', agent?.localIP||'', agent?.publicIP||'', log.event, log.details||'', log.uptime!=null?log.uptime:'', log.idle!=null?Math.round(log.idle/60):'', log.active!=null?Math.round(log.active/60):'', log.downtime!=null?log.downtime:'', log.currentState||'']);
    }
  }
  
  fs.writeFileSync(csvPath, rows.map(r => r.map(c => `"${String(c).replace(/"/g, '""')}"`).join(',')).join('\n'));
  
  const gitDir = __dirname;
  if (fs.existsSync(path.join(gitDir, '.git'))) {
    exec(`cd "${gitDir}" && git add logs/ && git diff --cached --quiet && git commit -m "Auto-update: Agent logs ${now.toISOString().slice(0, 10)}" || echo "nothing" && git push`, (err) => {
      if (err) console.log('GitHub push failed:', err.message);
      else console.log('Logs pushed to GitHub');
    });
  }
}, 6 * 60 * 60 * 1000);

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
          // Race condition prevention: check connectionId
          const newConnId = helloData.connectionId || String(Date.now());
          const existingAgent = agents.get(data.agentId);
          if (existingAgent && existingAgent.connectionId) {
            if (BigInt(existingAgent.connectionId) > BigInt(newConnId)) {
              console.log(`Stale connection rejected for ${data.agentId} (old: ${newConnId}, current: ${existingAgent.connectionId})`);
              ws.close(4004, 'Stale connection');
              return;
            }
          }
          const agentIP = helloData.agentIP || clientIp;
          const localIP = helloData.localIP || '';
          const publicIP = helloData.publicIP || '';
          const agentHostname = helloData.hostname || '';
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
            localIP,
            publicIP,
            hostname: agentHostname,
            connectedAt: Date.now(),
            connectionId: newConnId,
            events: [{type: 'connected', time: Date.now()}],
            bootTime: helloData.bootTime || '',
            programStart: helloData.programStart || '',
            totalIdle: helloData.totalIdle || 0,
            totalActive: helloData.totalActive || 0,
            version: helloData.version || '',
            currentState: helloData.currentState || 'active',
            currentIdle: helloData.currentIdle || 0
          });
          console.log(`Agent connected: ${agentName} (${data.agentId}) [local:${localIP} public:${publicIP}] (conn: ${clientIp})`);
          broadcastToDashboards({ type: 'agent-connected', agentId: data.agentId, name: agentName, ip: agentIP, localIP, publicIP, hostname: agentHostname });
          
          // Log system wake up event
          if (!agentLogs[data.agentId]) agentLogs[data.agentId] = [];
          const lastDisconnect = agentLogs[data.agentId].length > 0 ? agentLogs[data.agentId][agentLogs[data.agentId].length - 1] : null;
          let downtimeMin = '';
          if (lastDisconnect && lastDisconnect.event === 'system-sleep') {
            const downMs = Date.now() - new Date(lastDisconnect.timestamp).getTime();
            downtimeMin = Math.round(downMs / 60000);
          }
          agentLogs[data.agentId].push({
            timestamp: new Date().toISOString(),
            event: 'system-wake',
            details: `System woke up | Boot: ${helloData.bootTime || 'unknown'} | Downtime: ${downtimeMin != '' ? downtimeMin + 'min' : 'N/A'}`,
            hostname: agentHostname,
            localIP,
            publicIP,
            uptime: helloData.uptime || 0,
            idle: helloData.currentIdle || 0,
            active: helloData.totalActive || 0,
            currentState: 'active',
            downtime: downtimeMin
          });
          
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
            
            // Store log entry
            if (!agentLogs[data.agentId]) agentLogs[data.agentId] = [];
            agentLogs[data.agentId].push({
              timestamp: new Date().toISOString(),
              event: 'status-update',
              details: `State: ${sd.currentState}, Idle: ${sd.currentIdle}s, Uptime: ${sd.uptime}min`,
              uptime: sd.uptime,
              idle: sd.currentIdle,
              active: sd.totalActive,
              currentState: sd.currentState
            });
            
            // Keep only last 10000 logs per agent
            if (agentLogs[data.agentId].length > 10000) {
              agentLogs[data.agentId] = agentLogs[data.agentId].slice(-5000);
            }
            
            // Broadcast status to dashboards
            broadcastToDashboards({ type: 'agent-status', agentId: data.agentId, uptime: sd.uptime, totalIdle: sd.totalIdle, totalActive: sd.totalActive, currentState: sd.currentState });
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
            agentList.push({ id, name: a.name, viewers: a.viewers.size, ip: a.ip, localIP: a.localIP || '', publicIP: a.publicIP || '', hostname: a.hostname || '', org: a.org || '', uptime: a.uptime || 0, totalIdle: a.totalIdle || 0, totalActive: a.totalActive || 0, currentState: a.currentState || 'unknown' });
            if (a.org) orgList.add(a.org);
            // Auto-add dashboard as viewer of every agent (CCTV wall mode)
            a.viewers.add(ws);
          }
          ws.send(JSON.stringify({ type: 'agent-list', agents: agentList, orgs: [...orgList] }));
          console.log('Dashboard connected (CCTV wall)');
          break;

        // Dashboard wants to view an agent
        case 'view-agent':
          const targetAgent = agents.get(data.agentId);
          if (targetAgent) {
            if (ws.role !== 'dashboard') { ws.role = 'dashboard'; ws._id = Math.random().toString(36).slice(2); dashboards.add(ws); }
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

        // Support session: viewer connects with token
        case 'support-view':
          const session = supportSessions.get(data.token);
          if (!session || Date.now() > session.expiresAt) {
            ws.send(JSON.stringify({type: 'error', message: 'Session expired'}));
            ws.close(4005, 'Session expired');
            break;
          }
          if (data.agentId !== session.agentId) {
            ws.send(JSON.stringify({type: 'error', message: 'Agent mismatch'}));
            break;
          }
          const supportAgent = agents.get(data.agentId);
          if (!supportAgent) {
            ws.send(JSON.stringify({type: 'error', message: 'Agent not connected'}));
            break;
          }
          ws.role = 'support';
          ws.supportToken = data.token;
          ws.supportAgentId = data.agentId;
          supportAgent.viewers.add(ws);
          // Send current frame immediately
          if (supportAgent.lastFrame) {
            ws.send(JSON.stringify({type: 'frame', agentId: data.agentId, frame: supportAgent.lastFrame}));
          }
          // Notify agent to increase frame rate
          supportAgent.ws.send(JSON.stringify({type: 'set-fps', fps: 10}));
          console.log(`Support session started: ${data.agentId} (token: ${data.token.slice(0,8)}...)`);
          break;

        // Support session: request control
        case 'support-control':
          const sess = supportSessions.get(data.token);
          if (sess && sess.agentId === ws.supportAgentId) {
            sess.controlEnabled = data.enabled;
            console.log(`Support control ${data.enabled ? 'enabled' : 'disabled'} for ${sess.agentId}`);
          }
          break;

        // Control command from dashboard or support session
        case 'control':
          let ctrlAgentId = data.agentId;
          let allowed = false;
          if (ws.role === 'dashboard' && ctrlAgentId) {
            allowed = true;
            console.log(`Dashboard control allowed: ${ctrlAgentId} command: ${data.command}`);
          } else if (ws.role === 'support' && ws.supportToken) {
            const sess = supportSessions.get(ws.supportToken);
            if (sess && sess.controlEnabled && ctrlAgentId === sess.agentId) {
              allowed = true;
              console.log(`Support control allowed: ${ctrlAgentId} command: ${data.command}`);
            }
          }
          if (allowed && ctrlAgentId) {
            const ctrlTargetAgent = agents.get(ctrlAgentId);
            if (ctrlTargetAgent && ctrlTargetAgent.ws && ctrlTargetAgent.ws.readyState === WebSocket.OPEN) {
              ctrlTargetAgent.ws.send(JSON.stringify({
                type: 'control',
                command: data.command,
                params: data.params
              }));
              console.log(`Control sent to ${ctrlAgentId}: ${data.command}`);
            } else {
              console.log(`Control failed: agent ${ctrlAgentId} not connected or ws closed`);
            }
          } else {
            console.log(`Control denied: role=${ws.role} agentId=${ctrlAgentId} allowed=${allowed}`);
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
        
        // Log system sleep/shutdown event
        if (!agentLogs[ws.agentId]) agentLogs[ws.agentId] = [];
        const sessionMin = Math.round((Date.now() - (agent.connectedAt || Date.now())) / 60000);
        agentLogs[ws.agentId].push({
          timestamp: new Date().toISOString(),
          event: 'system-sleep',
          details: `System shutdown/sleep | Session: ${sessionMin}min | Last state: ${agent.currentState || 'unknown'} | Uptime: ${agent.uptime || 0}min`,
          uptime: agent.uptime || 0,
          idle: agent.totalIdle || 0,
          active: agent.totalActive || 0,
          currentState: 'offline',
          sessionDuration: sessionMin
        });
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