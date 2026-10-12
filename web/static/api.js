// API client. Sends the session token on every request; errors carry the
// server's {error} message, HTTP status and parsed body.

let token = '';

export class ApiError extends Error {
  constructor(message, status, data) {
    super(message);
    this.status = status;
    this.data = data;
  }
}

export async function initSession() {
  const res = await fetch('/api/session', { credentials: 'same-origin' });
  let data = null;
  try { data = await res.json(); } catch { /* not json */ }
  if (!res.ok || !data) {
    throw new ApiError((data && data.error) || 'No session. Open the URL printed by "loadmeter serve" (it contains ?token=...).', res.status, data);
  }
  token = data.token || '';
  // drop ?token= from the address bar once the cookie is set
  try {
    const u = new URL(location.href);
    if (u.searchParams.has('token')) { u.searchParams.delete('token'); history.replaceState(null, '', u.pathname + u.search + u.hash); }
  } catch { /* ignore */ }
  return data;
}

export async function api(method, path, body) {
  const headers = { 'X-LoadMeter-Token': token };
  let payload;
  if (typeof FormData !== 'undefined' && body instanceof FormData) {
    payload = body;
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, { method, headers, body: payload, credentials: 'same-origin' });
  } catch (e) {
    throw new ApiError('Cannot reach the Load Meter server: ' + e.message, 0, null);
  }
  const ct = res.headers.get('content-type') || '';
  let data = null;
  try { data = ct.includes('json') ? await res.json() : await res.text(); } catch { data = null; }
  if (!res.ok) {
    const msg = (data && typeof data === 'object' && data.error) || (typeof data === 'string' && data.trim().slice(0, 300)) || `HTTP ${res.status}`;
    throw new ApiError(msg, res.status, data);
  }
  return data;
}

export const get = (p) => api('GET', p);
export const post = (p, b) => api('POST', p, b === undefined ? {} : b);
export const put = (p, b) => api('PUT', p, b);
export const del = (p) => api('DELETE', p);
