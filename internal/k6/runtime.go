package k6

// runtimeJS is the inline helper library emitted into every exported script.
// It has no remote imports; it mirrors Load Meter's ${var}/${__fn()} expansion,
// extractors and assertion helpers. No backticks may appear in it.
const runtimeJS = `// ---- Load Meter runtime helpers (inline, no remote imports) ----
const _ALNUM = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
let _counter = 0;
const _jc = { body: null, doc: undefined };

function _uuid() {
  const h = '0123456789abcdef';
  let s = '';
  for (let i = 0; i < 36; i++) {
    if (i === 8 || i === 13 || i === 18 || i === 23) s += '-';
    else if (i === 14) s += '4';
    else if (i === 19) s += h[8 + Math.floor(Math.random() * 4)];
    else s += h[Math.floor(Math.random() * 16)];
  }
  return s;
}

function _pad(n, l) { return String(n).padStart(l || 2, '0'); }

function _time(f) {
  if (!f) return String(Date.now());
  const d = new Date();
  return f.replace(/yyyy|MM|dd|HH|mm|ss|SSS/g, function (t) {
    switch (t) {
      case 'yyyy': return _pad(d.getFullYear(), 4);
      case 'MM': return _pad(d.getMonth() + 1);
      case 'dd': return _pad(d.getDate());
      case 'HH': return _pad(d.getHours());
      case 'mm': return _pad(d.getMinutes());
      case 'ss': return _pad(d.getSeconds());
      default: return _pad(d.getMilliseconds(), 3);
    }
  });
}

function _urlenc(s) {
  return encodeURIComponent(s).replace(/[!'()*]/g, function (c) { return '%' + c.charCodeAt(0).toString(16).toUpperCase(); });
}

function _call(expr, vars) {
  let name = expr, argStr = '';
  const p = expr.indexOf('(');
  if (p >= 0) {
    if (expr[expr.length - 1] !== ')') return undefined;
    name = expr.slice(0, p);
    argStr = expr.slice(p + 1, expr.length - 1);
  }
  const args = argStr === '' ? [] : argStr.split(',').map(function (a) { return _expand(a.trim(), vars); });
  switch (name) {
    case '__uuid': return _uuid();
    case '__randomInt': {
      const lo = parseInt(args[0], 10), hi = parseInt(args[1], 10);
      if (args.length !== 2 || isNaN(lo) || isNaN(hi) || hi < lo) return undefined;
      return String(lo + Math.floor(Math.random() * (hi - lo + 1)));
    }
    case '__randomString': {
      let n = parseInt(args[0], 10);
      if (!(n > 0 && n <= 4096)) n = 8;
      let s = '';
      for (let i = 0; i < n; i++) s += _ALNUM[Math.floor(Math.random() * _ALNUM.length)];
      return s;
    }
    case '__timestamp': return _time(args[0]);
    case '__counter': return String(++_counter);
    case '__vu': return String(__VU);
    case '__iteration': return String(__ITER);
    case '__env': {
      if (args.length < 1 || args[0].indexOf('LOADMETER_') !== 0) return undefined;
      const v = __ENV[args[0]];
      return v !== undefined ? v : (args.length > 1 ? args[1] : '');
    }
    case '__urlencode': return args.length < 1 ? undefined : _urlenc(args.join(','));
  }
  return undefined;
}

function _expand(s, vars) {
  if (typeof s !== 'string' || s.indexOf('${') < 0) return s;
  let out = '';
  for (;;) {
    const i = s.indexOf('${');
    if (i < 0) { out += s; break; }
    out += s.slice(0, i);
    let depth = 1, j = i + 2;
    for (; j < s.length; j++) {
      const c = s[j];
      if (c === '{') depth++;
      else if (c === '}') { depth--; if (depth === 0) break; }
    }
    if (j >= s.length) { out += s.slice(i); break; }
    const expr = s.slice(i + 2, j);
    let v;
    if (expr.slice(0, 2) === '__') v = _call(expr, vars);
    else if (Object.prototype.hasOwnProperty.call(vars, expr)) v = vars[expr];
    out += v === undefined ? s.slice(i, j + 1) : v;
    s = s.slice(j + 1);
  }
  return out;
}

function _unq(s) { return s.replace(/^["']+|["']+$/g, ''); }

function _cond(c, vars) {
  const ops = ['==', '!='];
  for (let k = 0; k < ops.length; k++) {
    const i = c.indexOf(ops[k]);
    if (i >= 0) {
      const l = _unq(_expand(c.slice(0, i).trim(), vars)), r = _unq(_expand(c.slice(i + 2).trim(), vars));
      return ops[k] === '==' ? l === r : l !== r;
    }
  }
  const v = _expand(c.trim(), vars).trim();
  if (v.indexOf('${') >= 0) return false;
  return v !== '' && v !== 'false' && v !== '0';
}

function _str(v) {
  if (v === undefined) return undefined;
  if (v !== null && typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

function _set(vars, name, v) { if (v !== undefined && v !== null) vars[name] = _str(v); }

// JSONPath subset: $, .name, ['name'], [n]
function _jp(body, expr) {
  let doc;
  if (_jc.body === body) doc = _jc.doc;
  else {
    try { doc = JSON.parse(body); } catch (e) { doc = undefined; }
    _jc.body = body; _jc.doc = doc;
  }
  if (doc === undefined) return undefined;
  const rest = expr.replace(/^\$/, '');
  const re = /\.([A-Za-z_$][\w$-]*)|\[(\d+)\]|\['([^']*)'\]|\["([^"]*)"\]/g;
  let m, last = 0, cur = doc;
  while ((m = re.exec(rest)) !== null) {
    if (m.index !== last) return undefined;
    last = re.lastIndex;
    const t = m[1] !== undefined ? m[1] : m[2] !== undefined ? Number(m[2]) : m[3] !== undefined ? m[3] : m[4];
    if (cur === null || typeof cur !== 'object' || !(t in cur)) return undefined;
    cur = cur[t];
  }
  return last === rest.length ? cur : undefined;
}

function _rx(text, pat, flags, group, match, def) {
  let re;
  try { re = new RegExp(pat, (flags || '') + 'g'); } catch (e) { return def; }
  const all = [];
  let m;
  while ((m = re.exec(text)) !== null) { all.push(m); if (m[0] === '') re.lastIndex++; }
  if (!all.length) return def;
  const pick = match === -1 ? all[Math.floor(Math.random() * all.length)] : all[Math.max(match, 1) - 1];
  if (!pick) return def;
  const g = group || (pick.length > 1 ? 1 : 0);
  return pick[g] === undefined ? def : pick[g];
}

function _bnd(text, l, r, match, def) {
  const found = [];
  let pos = 0;
  while (found.length < 1000) {
    const a = text.indexOf(l, pos);
    if (a < 0) break;
    const s = a + l.length, b = text.indexOf(r, s);
    if (b < 0) break;
    found.push(text.slice(s, b));
    pos = b + r.length;
  }
  if (!found.length) return def;
  const v = match === -1 ? found[Math.floor(Math.random() * found.length)] : found[Math.max(match, 1) - 1];
  return v === undefined ? def : v;
}

function _hdr(res, name) {
  const want = name.toLowerCase();
  for (const k in res.headers) if (k.toLowerCase() === want) return res.headers[k];
  return undefined;
}

function _cookie(res, name) {
  const c = res.cookies && res.cookies[name];
  return c && c.length ? c[c.length - 1].value : undefined;
}

function _stat(code, spec) {
  return spec.split(',').some(function (p) {
    p = p.trim().toLowerCase();
    if (/^\dxx$/.test(p)) return Math.floor(code / 100) === Number(p[0]);
    return code === Number(p);
  });
}

function _think(minMs, maxMs, scale) {
  const ms = (minMs + Math.random() * Math.max(0, maxMs - minMs)) * scale;
  if (ms > 0) sleep(ms / 1000);
}

function _form(pairs, vars) {
  return pairs.map(function (kv) { return _urlenc(_expand(kv[0], vars)) + '=' + _urlenc(_expand(kv[1], vars)); }).join('&');
}

function _multipart(parts, vars) {
  const b = '----LoadMeterBoundary' + Math.random().toString(16).slice(2);
  let body = '';
  parts.forEach(function (p) {
    body += '--' + b + '\r\nContent-Disposition: form-data; name="' + _expand(p[0], vars).replace(/"/g, '%22') + '"\r\n\r\n' + _expand(p[1], vars) + '\r\n';
  });
  return { body: body + '--' + b + '--\r\n', type: 'multipart/form-data; boundary=' + b };
}

function _csv(text, delim, columns) {
  const rows = [];
  let row = [], field = '', q = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (q) {
      if (c === '"') { if (text[i + 1] === '"') { field += '"'; i++; } else q = false; }
      else field += c;
    } else if (c === '"') q = true;
    else if (c === delim) { row.push(field); field = ''; }
    else if (c === '\n' || c === '\r') {
      if (c === '\r' && text[i + 1] === '\n') i++;
      row.push(field); field = '';
      if (row.length > 1 || row[0] !== '') rows.push(row);
      row = [];
    } else field += c;
  }
  if (field !== '' || row.length) { row.push(field); rows.push(row); }
  return _rows(rows, columns);
}

function _rows(rows, columns) {
  const cols = columns && columns.length ? columns : rows.shift();
  if (!cols) return [];
  return rows.map(function (r) {
    const o = {};
    cols.forEach(function (c, i) { o[c] = r[i] === undefined ? '' : r[i]; });
    return o;
  });
}

const _vuRow = {};
// Returns 'ok', 'skip' (iteration ends quietly) or throws via fail().
function _feed(name, rows, mode, scope, onExhaust, vars) {
  if (scope === 'vu' && _vuRow[name]) return 'ok';
  if (!rows.length) return 'skip';
  let idx;
  if (mode === 'random') idx = Math.floor(Math.random() * rows.length);
  else if (mode === 'per-vu') idx = (exec.vu.idInTest - 1) % rows.length;
  else idx = exec.scenario.iterationInTest;
  if (idx >= rows.length) {
    if (onExhaust === 'stop') return 'skip';
    if (onExhaust === 'error') fail('data source ' + name + ' exhausted');
    idx = idx % rows.length;
  }
  const row = rows[idx];
  for (const k in row) vars[k] = row[k];
  _vuRow[name] = true;
  return 'ok';
}

function _req(vars, s) {
  let u = _expand(s.url, vars);
  if (u.indexOf('://') < 0) {
    const b = _expand(BASE_URL, vars).replace(/\/+$/, '');
    u = b + (u[0] === '/' ? u : '/' + u);
  }
  const hk = {};
  DEFAULT_HEADERS.concat(s.headers || []).forEach(function (h) { hk[h[0].toLowerCase()] = [h[0], _expand(h[1], vars)]; });
  let body = null;
  if (s.form) {
    body = _form(s.form, vars);
    if (!hk['content-type']) hk['content-type'] = ['Content-Type', 'application/x-www-form-urlencoded'];
  } else if (s.multipart) {
    const mp = _multipart(s.multipart, vars);
    body = mp.body;
    hk['content-type'] = ['Content-Type', mp.type];
  } else if (s.fileBody) {
    body = {};
    s.fileBody.forEach(function (p) { body[_expand(p[0], vars)] = p[2] ? http.file(p[2].data, p[2].name, p[2].type) : _expand(p[1], vars); });
  } else if (s.body !== undefined) body = _expand(s.body, vars);
  const headers = {};
  for (const k in hk) headers[hk[k][0]] = hk[k][1];
  const params = { headers: headers, tags: { name: s.name } };
  const to = s.timeout || DEFAULT_TIMEOUT;
  if (to) params.timeout = to;
  if (s.redirects !== undefined) params.redirects = s.redirects;
  else if (DEFAULT_REDIRECTS !== undefined) params.redirects = DEFAULT_REDIRECTS;
  if (NO_COOKIES) params.jar = new http.CookieJar();
  return http.request(s.method, u, body, params);
}
// ---- end of runtime helpers ----
`
