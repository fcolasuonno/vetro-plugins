'use strict';
// Surfshark (WireGuard) on the TV through surfsharktv, the WireGuard client built from ../surfsharktv (surfsharktv/build.sh).
// A plugin bundle is text only, so the 2.8 MB binary is not inside it:
//  - /opt/surfsharktv/surfsharktv is used when it is there (installed by hand, or by an earlier version).
//  - When it is missing, or was installed by an older plugin version, the plugin downloads BINARY.url, refuses it
//    unless its SHA-256 is BINARY.sha256, unpacks it and records the checksum in /opt/surfsharktv/.source. This runs at
//    start and from the page, never on every call, so a slow download cannot hold Vetro's command queue.
//    NOTE: this first version ships a PLACEHOLDER url/sha256. Set both to the real surfsharktv release before publishing.
//  - Before each command it is copied into a read-only bind mount under /opt and run from there: newer Tizen
//    firmware enforces UEP, which refuses unsigned ELFs on writable filesystems but skips read-only VDFS mounts.
//    start() (re)creates that mount, so it comes back after every boot with this Shard.
// State lives in the binary's own files (/opt/surfsharktv/config, wg.key, wg.conf, log); the same commands work from
// a shell. The WireGuard private key/config is written through base64 and never read back.
// Plain ES5: older TVs run Node 4.4.
var fs = require('fs'), path = require('path'), http = require('http'), url = require('url');
// TODO(release): replace the placeholder URL and SHA-256 below with the real pinned surfsharktv release, then rebuild.
var BINARY = { url: 'https://github.com/piecifra/surfsharktv/releases/download/v1.0.0/surfsharktv.gz', sha256: '0000000000000000000000000000000000000000000000000000000000000000' };
var DIR = '/opt/surfsharktv', RO = DIR + '/ro', RUN = RO + '/surfsharktv';
var COUNTRY = /^[A-Z]{2}$/, SHA = /^[0-9a-f]{64}$/, URL_RE = /^https:\/\/[A-Za-z0-9.\/_~%+-]+$/;
var RETRY_MS = 120000, TICK_MS = 60000, FIRST_TICK_MS = 15000, MAX_BODY = 8192;

// /opt/surfsharktv holds a binary that runs as root and the WireGuard key/config: root only. Folder 0700, files 0600, the
// binary 0700 (the TV's default umask had left the folder 0777). Vetro reaches it only through the root agent.
// UEP: the TV's kernel refuses unsigned ELFs on writable mounts, but skips read-only VDFS mounts. So the running copy
// lives in RO, a bind mount of /opt/surfsharktv/ro remounted read-only. Recreate it if it is missing or the binary changed.
var STAGE = 'chmod 700 ' + DIR + ' 2>/dev/null; chmod 600 ' + DIR + '/* 2>/dev/null; chmod 700 ' + DIR + '/surfsharktv 2>/dev/null\n' +
    '[ -x ' + DIR + '/surfsharktv ] || { echo __NOBIN__; exit 0; }\n' +
    'if grep -q " ' + RO + ' " /proc/mounts 2>/dev/null; then a=$(md5sum < ' + DIR + '/surfsharktv); b=$(md5sum < ' + RUN + ' 2>/dev/null); [ "$a" = "$b" ] || { umount ' + RO + ' 2>/dev/null; rm -f ' + RUN + '; }; fi\n' +
    'mkdir -p ' + RO + ' && chmod 700 ' + RO + '\n' +
    'if [ ! -x ' + RUN + ' ]; then cp ' + DIR + '/surfsharktv ' + RUN + ' && chmod 700 ' + RUN + ' && mount --bind ' + RO + ' ' + RO + ' && mount -o remount,ro,bind ' + RO + '; fi\n' +
    '[ -x ' + RUN + ' ] || { echo __NOBIN__; exit 0; }\ncd /\n';

// Node 4.4 has a Buffer.from inherited from Uint8Array that does not take a string; Node 5.10+ has the real one.
function bytes(s) { return Buffer.from && Buffer.from !== Uint8Array.from ? Buffer.from(s) : new Buffer(s); }
function bytesB64(s) { return Buffer.from && Buffer.from !== Uint8Array.from ? Buffer.from(s, 'base64') : new Buffer(s, 'base64'); }
function installScript(bin) {
    if (!bin || !URL_RE.test(bin.url || '') || !SHA.test(bin.sha256 || '')) return null;
    var tmp = DIR + '/surfsharktv.gz.part';
    return 'umask 077\nmkdir -p ' + DIR + ' && chmod 700 ' + DIR + '\n' +
        'if [ -x ' + DIR + '/surfsharktv ] && [ "$(cat ' + DIR + '/.source 2>/dev/null)" = ' + bin.sha256 + ' ]; then echo __CURRENT__; exit 0; fi\n' +
        'rm -f ' + tmp + '\n' +
        'curl -fsSL --max-time 90 -o ' + tmp + ' \'' + bin.url + '\' || { rm -f ' + tmp + '; echo __DOWNLOAD_FAILED__; exit 0; }\n' +
        'if [ "$(sha256sum < ' + tmp + ' | cut -d" " -f1)" != ' + bin.sha256 + ' ]; then rm -f ' + tmp + '; echo __CHECKSUM_MISMATCH__; exit 0; fi\n' +
        'gunzip -c ' + tmp + ' > ' + DIR + '/surfsharktv.new && chmod 700 ' + DIR + '/surfsharktv.new && mv -f ' + DIR + '/surfsharktv.new ' + DIR + '/surfsharktv && echo ' + bin.sha256 + ' > ' + DIR + '/.source && rm -f ' + tmp + ' && echo __INSTALLED__\n';
}

function wrap(s) { return 'echo ' + bytes(s).toString('base64') + ' | base64 -d | sh'; }
function lastJson(out) {
    var lines = String(out || '').trim().split('\n');
    for (var i = lines.length - 1; i >= 0; i--) { var l = lines[i].trim(); if (l.charAt(0) === '{') { try { return JSON.parse(l); } catch (e) { /* not JSON */ } } }
    return null;
}
function failure(r, fallback) {
    var text = String(r && (r.stderr || r.stdout) || '').trim().split('\n').filter(function (l) { return l && l.indexOf('sendmmsg') < 0; }).slice(-3).join(' · ');
    return new Error(text || fallback);
}
function validate(o) {
    o = o || {}; var out = {};
    if (o.country !== undefined) { if (o.country !== '' && !COUNTRY.test(o.country)) throw new Error('Invalid country code'); out.country = o.country; }
    ['killswitch', 'autostart'].forEach(function (k) { if (o[k] !== undefined) { if (typeof o[k] !== 'boolean') throw new Error('Invalid ' + k); out[k] = o[k]; } });
    return out;
}
function settingsScript(o) {
    var s = '';
    if (o.country !== undefined) s += RUN + ' set country "' + o.country + '" >/dev/null\n';
    if (o.killswitch !== undefined) s += RUN + ' set killswitch ' + (o.killswitch ? 'on' : 'off') + ' >/dev/null\n';
    if (o.autostart !== undefined) s += RUN + ' set autostart ' + (o.autostart ? 'on' : 'off') + ' >/dev/null\n';
    return s;
}

function Vpn(execute, bin) { this.execute = execute; this.bin = bin || BINARY; this.busy = false; this.countryList = null; this.autoFailedAt = 0; this.lastAuto = null; this.lastInstall = null; this.last = null; }
function lock(self, work) {
    if (self.busy) return Promise.reject(new Error('VPN change already in progress'));
    self.busy = true;
    return Promise.resolve().then(work).then(function (v) { self.busy = false; return v; }, function (e) { self.busy = false; throw e; });
}
Vpn.prototype.install = function () {
    var self = this, script = installScript(this.bin);
    if (!script) { this.lastInstall = { at: Date.now(), result: 'no-download' }; return Promise.resolve(this.lastInstall); }
    return this.execute(wrap(script), 120000).then(function (r) {
        var m = /__(CURRENT|INSTALLED|DOWNLOAD_FAILED|CHECKSUM_MISMATCH)__/.exec(r.stdout || '');
        self.lastInstall = { at: Date.now(), result: m ? m[1].toLowerCase().replace('_', '-') : 'failed' };
        if (!m) self.lastInstall.error = failure(r, 'Install failed').message;
        return self.lastInstall;
    });
};
Vpn.prototype.read = function (withIp) {
    var self = this;
    return this.execute(wrap(STAGE + RUN + ' ' + (withIp ? 'status -json' : 'state')), withIp ? 20000 : 10000).then(function (r) {
        if (/__NOBIN__/.test(r.stdout || '')) return (self.last = { installed: false, canDownload: !!installScript(self.bin), lastInstall: self.lastInstall });
        var s = lastJson(r.stdout); if (!s) throw failure(r, 'VPN state unreadable');
        s.installed = true; s.lastAuto = self.lastAuto; s.lastInstall = self.lastInstall; s.canDownload = !!installScript(self.bin);
        return (self.last = s);
    });
};
Vpn.prototype.countries = function () {
    var self = this;
    if (this.countryList) return Promise.resolve(this.countryList);
    return this.execute(wrap(STAGE + RUN + ' countries'), 25000).then(function (r) {
        if (/__NOBIN__/.test(r.stdout || '')) throw new Error('The VPN program is not installed');
        var list = String(r.stdout || '').split('\n').map(function (l) { var p = l.split('\t'); return p.length === 2 && COUNTRY.test(p[0]) ? { code: p[0], name: p[1].trim() } : null; }).filter(Boolean);
        if (!list.length) throw failure(r, 'Country list unavailable');
        return (self.countryList = list);
    });
};
Vpn.prototype.start = function (opts) {
    var self = this, o;
    try { o = validate(opts); } catch (e) { return Promise.reject(e); }
    return lock(this, function () {
        return self.execute(wrap(STAGE + settingsScript(o) + RUN + ' start 2>&1'), 90000).then(function (r) {
            if (/__NOBIN__/.test(r.stdout || '')) throw new Error('The VPN program is not installed');
            return self.read(true).then(function (s) {
                if (!s.running) throw failure(r, 'VPN did not start');
                if (o.country !== undefined && s.country !== o.country) throw new Error('Country not confirmed');
                if (o.killswitch !== undefined && s.killswitch !== o.killswitch) throw new Error('Kill switch not confirmed');
                return s;
            });
        });
    });
};
Vpn.prototype.stop = function () {
    var self = this;
    return lock(this, function () {
        return self.execute(wrap(STAGE + RUN + ' stop 2>&1'), 30000).then(function () { return self.read(false); }).then(function (s) {
            if (s.running) throw new Error('VPN still running');
            return s;
        });
    });
};
Vpn.prototype.configure = function (opts) {
    var self = this, o;
    try { o = validate(opts); } catch (e) { return Promise.reject(e); }
    return this.read(false).then(function (s) {
        if (!s.installed) throw new Error('The VPN program is not installed');
        var restart = s.running && (o.country !== undefined && o.country !== s.country || o.killswitch !== undefined && o.killswitch !== s.killswitch);
        if (restart) return self.start(o);
        return lock(self, function () { return self.execute(wrap(STAGE + settingsScript(o)), 15000).then(function () { return self.read(false); }); });
    });
};
// A WireGuard private key registered on surfshark.com lets the TV pick a server per country from the public list, so
// the country picker works without the login API (which Cloudflare blocks from many TV/office networks).
Vpn.prototype.setKey = function (key) {
    var self = this;
    if (typeof key !== 'string') return Promise.reject(new Error('Invalid WireGuard private key'));
    var k = key.trim();
    if (!k || k.length > 128 || bytesB64(k).length !== 32) return Promise.reject(new Error('Invalid WireGuard private key (need 32 base64-encoded bytes)'));
    var b64 = bytes(k + '\n').toString('base64');
    return lock(this, function () {
        return self.execute(wrap(STAGE + 'umask 077\nmkdir -p ' + DIR + '\necho ' + b64 + ' | base64 -d > ' + DIR + '/wg.key.new && chmod 600 ' + DIR + '/wg.key.new && mv -f ' + DIR + '/wg.key.new ' + DIR + '/wg.key && rm -f ' + DIR + '/wg.conf\n' + RUN + ' verify'), 30000).then(function (r) {
            if (/__NOBIN__/.test(r.stdout || '')) throw new Error('The VPN program is not installed');
            var v = lastJson(r.stdout); if (!v) throw failure(r, 'Key check unavailable');
            return self.read(false).then(function (s) { s.keyCheck = v; return s; });
        });
    });
};
// A full WireGuard config from Surfshark's manual-setup page pins one server (and ignores the country picker).
Vpn.prototype.setConfig = function (conf) {
    var self = this;
    if (typeof conf !== 'string' || !conf.trim() || conf.length > 16384) return Promise.reject(new Error('Invalid WireGuard config'));
    if (!/\[Interface\]/.test(conf) || !/\[Peer\]/.test(conf) || !/PrivateKey\s*=/.test(conf) || !/PublicKey\s*=/.test(conf) || !/Endpoint\s*=/.test(conf)) return Promise.reject(new Error('Not a WireGuard config: need [Interface], [Peer], PrivateKey, PublicKey and Endpoint'));
    var b64 = bytes(conf.trim() + '\n').toString('base64');
    return lock(this, function () {
        return self.execute(wrap(STAGE + 'umask 077\nmkdir -p ' + DIR + '\necho ' + b64 + ' | base64 -d > ' + DIR + '/wg.conf.new && chmod 600 ' + DIR + '/wg.conf.new && mv -f ' + DIR + '/wg.conf.new ' + DIR + '/wg.conf && rm -f ' + DIR + '/wg.key\n' + RUN + ' verify'), 30000).then(function (r) {
            if (/__NOBIN__/.test(r.stdout || '')) throw new Error('The VPN program is not installed');
            var v = lastJson(r.stdout); if (!v) throw failure(r, 'Config check unavailable');
            return self.read(false).then(function (s) { s.configCheck = v; return s; });
        });
    });
};
// Starts the tunnel only when it was left on (enabled) with "keep connected" (autostart) and a key or config. Never stops it.
Vpn.prototype.supervise = function () {
    var self = this;
    if (this.busy || Date.now() - this.autoFailedAt < RETRY_MS) return Promise.resolve({ started: false });
    return this.read(false).then(function (s) {
        if (!s.installed || s.running || !s.enabled || !s.autostart || (!s.hasConfig && !s.hasPrivateKey)) return { started: false };
        return self.start({}).then(function (after) { self.lastAuto = { at: Date.now(), ok: true }; return { started: true, state: after }; },
            function (e) { self.autoFailedAt = Date.now(); self.lastAuto = { at: Date.now(), ok: false, error: e.message }; throw e; });
    });
};

function json(res, body, code) {
    res.writeHead(code || 200, { 'content-type': 'application/json', 'cache-control': 'no-store' });
    res.end(JSON.stringify(body));
}
function readJson(req, cb) {
    var chunks = [], size = 0, done = false;
    req.on('data', function (c) { size += c.length; if (size > MAX_BODY) { if (!done) { done = true; cb(new Error('Request too large')); } return; } chunks.push(c); });
    req.on('end', function () { if (done) return; done = true; var p; try { p = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}'); } catch (e) { return cb(new Error('Invalid JSON')); } if (!p || typeof p !== 'object' || Array.isArray(p)) return cb(new Error('Invalid JSON')); cb(null, p); });
    req.on('error', function (e) { if (!done) { done = true; cb(e); } });
}

// The page ships next to this file. ctx.dir is the plugin's folder on the TV, but a test harness may give an empty
// one: read it from where the code is, fall back to ctx.dir, and never fail start() over it.
function loadPage(ctx) {
    var places = [path.join(__dirname, 'ui.html'), path.join(ctx.dir || '', 'ui.html')];
    for (var i = 0; i < places.length; i++) { try { return fs.readFileSync(places[i], 'utf8'); } catch (e) { /* next */ } }
    return '<!doctype html><meta charset="utf-8"><p>Page file missing: reinstall this Shard.</p>';
}
// A small local HTTP bridge for the optional Tizen "Surf Control" app. Vetro's dashboard API refuses
// cross-origin requests, so the app talks to this instead. CORS is open; the TV webview reaches it via the LAN IP.
var CONTROL_PORT = 18765;
function control(req, res, vpn, queue) {
    var u = url.parse(req.url || '/', true);
    res.setHeader('access-control-allow-origin', '*');
    res.setHeader('access-control-allow-methods', 'GET,POST,OPTIONS');
    res.setHeader('access-control-allow-headers', 'Content-Type');
    if (req.method === 'OPTIONS') { res.writeHead(204); res.end(); return; }
    // Cached state, refreshed by the supervisor and by any dashboard action. Never call ctx.execute from an HTTP
    // callback here: Vetro's JS host does not resolve those promises, so commands are queued for the timer loop.
    if (u.pathname === '/status' && req.method === 'GET') { json(res, vpn.last || { installed: true }); return; }
    if (u.pathname === '/countries' && req.method === 'GET') {
        if (vpn.countryList) { json(res, { countries: vpn.countryList }); return; }
        queue.push({ kind: 'countries' });
        json(res, { ok: true, queued: true });
        return;
    }
    if (req.method === 'POST') {
        readJson(req, function (err, p) {
            if (err) return json(res, { ok: false, error: err.message }, 400);
            if (['/on', '/off', '/country', '/killswitch', '/autostart'].indexOf(u.pathname) < 0) return json(res, { ok: false, error: 'Unknown command' }, 404);
            queue.push({ path: u.pathname, p: p });
            json(res, { ok: true, queued: true });
        });
        return;
    }
    json(res, { ok: false, error: 'Not found' }, 404);
}

module.exports = function (ctx) {
    var vpn = new Vpn(ctx.execute), page = null, timer = null, first = null, stopped = true, controlServer = null, queue = [], drainTimer = null;
    function tick() { if (stopped) return; vpn.supervise().catch(function () { /* recorded in lastAuto; the root agent may not be up yet */ }); }
    function reply(res, p) { p.then(function (d) { json(res, d); }, function (e) { json(res, { ok: false, error: String(e && e.message || e) }, 409); }); }
    function drain() {
        if (stopped || vpn.busy || !queue.length) return;
        var job = queue.shift(), t;
        try {
            if (job.kind === 'countries') t = vpn.countries();
            else if (job.path === '/on') t = vpn.start({});
            else if (job.path === '/off') t = vpn.stop();
            else if (job.path === '/country') t = vpn.configure({ country: job.p.country });
            else if (job.path === '/killswitch') t = vpn.configure({ killswitch: !!job.p.on });
            else if (job.path === '/autostart') t = vpn.configure({ autostart: !!job.p.on });
        } catch (e) { /* malformed job: drop it */ }
        if (t && t.catch) t.catch(function () { /* state keeps the last error */ });
    }
    return {
        start: function () {
            stopped = false;
            page = loadPage(ctx);
            // Install (or update) the binary first, then begin supervising; neither may throw out of start().
            first = setTimeout(function () { vpn.install().catch(function () {}).then(tick); }, FIRST_TICK_MS);
            timer = setInterval(tick, TICK_MS);
            if (timer.unref) timer.unref();
            drainTimer = setInterval(drain, 1000);
            if (drainTimer.unref) drainTimer.unref();
            try {
                controlServer = http.createServer(function (req, res) { control(req, res, vpn, queue); });
                controlServer.on('error', function () { controlServer = null; });
                controlServer.listen(CONTROL_PORT, '0.0.0.0');
            } catch (e) { controlServer = null; }
        },
        // Switching the Shard off takes the VPN down with it: `surfsharktv down` stops the tunnel and removes every route,
        // rule, firewall chain and the surftun interface it made, but keeps "VPN on" saved. Switching the Shard on
        // again, or Vetro restarting, brings it back when "Keep connected" is on. Only the page's VPN switch
        // (`surfsharktv stop`) turns it off for good.
        stop: function () {
            stopped = true; clearTimeout(first); clearInterval(timer); clearInterval(drainTimer); queue = []; page = null;
            if (controlServer) { try { controlServer.close(); } catch (e) { /* already gone */ } controlServer = null; }
            try { ctx.execute(wrap('[ -x ' + RUN + ' ] && ' + RUN + ' down >/dev/null 2>&1; true'), 30000).catch(function () { /* nothing to undo */ }); } catch (e) { /* no exec: nothing ran */ }
        },
        status: function () { var s = vpn.last || {}; return { running: !!s.running, server: s.server || null, installed: s.installed !== false }; },
        route: function (req, res, sub) {
            if (sub === 'ui' && req.method === 'GET') { res.writeHead(200, { 'content-type': 'text/html; charset=utf-8', 'cache-control': 'no-store' }); res.end(page || ''); return true; }
            if (sub === 'state' && req.method === 'GET') { reply(res, vpn.read(true)); return true; }
            if (sub === 'countries' && req.method === 'GET') { reply(res, vpn.countries().then(function (c) { return { countries: c }; })); return true; }
            if (sub === 'action' && req.method === 'POST') {
                readJson(req, function (err, p) {
                    if (err) return json(res, { ok: false, error: err.message }, 400);
                    var t = p.action === 'start' ? vpn.start(p.settings) : p.action === 'stop' ? vpn.stop() : p.action === 'configure' ? vpn.configure(p.settings)
                        : p.action === 'config' ? vpn.setConfig(p.conf) : p.action === 'key' ? vpn.setKey(p.key)
                        : p.action === 'install' ? vpn.install().then(function () { return vpn.read(false); })
                        : Promise.reject(new Error('Invalid VPN action'));
                    reply(res, t);
                });
                return true;
            }
            return false;
        }
    };
};
module.exports.Vpn = Vpn;
module.exports.validate = validate;
module.exports.installScript = installScript;
module.exports.STAGE = STAGE;
module.exports.BINARY = BINARY;
