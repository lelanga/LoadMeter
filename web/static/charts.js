// Canvas 2D line charts: responsive, device-pixel-ratio aware, hover tooltip.
import { h } from './ui.js';

const live = new Set();
try {
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => live.forEach((c) => { if (c.isAttached()) c.draw(); else live.delete(c); }));
} catch { /* old browser */ }

const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

function niceMax(v) {
  if (!(v > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / p;
  const m = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10;
  return m * p;
}

// opts: {title, height, series:[{key,label,color:'--c1',axis:'left'|'right',fmt(v),get(point)}], xLabel}
export function createChart(opts) {
  const canvas = h('canvas', { role: 'img', 'aria-label': opts.title, tabindex: '0' });
  const tip = h('div', { class: 'chart-tip', hidden: true });
  const wrap = h('div', { class: 'chart-wrap' }, canvas, tip);
  if (opts.height) canvas.style.height = opts.height + 'px';
  const legend = h('div', { class: 'legend' }, opts.series.map((s) => {
    const i = h('i', {});
    i.style.background = `var(${s.color})`;
    return h('span', {}, i, s.label);
  }));
  const el = h('div', {}, h('h3', {}, opts.title), wrap, legend);
  const ctx = canvas.getContext('2d');
  let pts = [];
  let hover = -1;
  let W = 0, H = 0, dpr = 1;
  const pad = { l: 54, r: 12, t: 10, b: 22 };

  const chart = { el, setData, draw, isAttached: () => canvas.isConnected };

  function setData(points) {
    pts = points || [];
    const last = pts[pts.length - 1];
    if (last) {
      canvas.setAttribute('aria-label', opts.title + ': ' + opts.series.map((s) => `${s.label} ${s.fmt(s.get(last))}`).join(', ') + ` at ${last.t} seconds`);
    }
    draw();
  }

  function layout() {
    dpr = window.devicePixelRatio || 1;
    const r = canvas.getBoundingClientRect();
    W = Math.max(50, Math.round(r.width));
    H = Math.max(50, Math.round(r.height));
    if (canvas.width !== Math.round(W * dpr) || canvas.height !== Math.round(H * dpr)) {
      canvas.width = Math.round(W * dpr);
      canvas.height = Math.round(H * dpr);
    }
  }

  function scales() {
    const hasRight = opts.series.some((s) => s.axis === 'right');
    const l = pad.l, r = hasRight ? 50 : pad.r;
    const t0 = pts.length ? pts[0].t : 0;
    const t1 = pts.length ? pts[pts.length - 1].t : 1;
    const span = Math.max(t1 - t0, 1);
    const max = { left: 0, right: 0 };
    for (const s of opts.series) for (const p of pts) { const v = Number(s.get(p)); if (v > max[s.axis || 'left']) max[s.axis || 'left'] = v; }
    max.left = niceMax(max.left); max.right = niceMax(max.right);
    const x = (t) => l + ((t - t0) / span) * (W - l - r);
    const y = (v, ax) => H - pad.b - (v / max[ax || 'left']) * (H - pad.t - pad.b);
    return { x, y, max, l, r, t0, t1, hasRight };
  }

  function draw() {
    if (!canvas.isConnected) { return; }
    layout();
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    const sc = scales();
    const grid = cssVar('--grid'), muted = cssVar('--muted');
    ctx.font = '11px system-ui, sans-serif';
    ctx.lineWidth = 1;
    // y grid + labels (left axis; right axis labelled on the right)
    for (let i = 0; i <= 4; i++) {
      const gy = pad.t + ((H - pad.t - pad.b) * i) / 4;
      ctx.strokeStyle = grid; ctx.beginPath(); ctx.moveTo(sc.l, gy + .5); ctx.lineTo(W - sc.r, gy + .5); ctx.stroke();
      const frac = 1 - i / 4;
      ctx.fillStyle = muted; ctx.textBaseline = 'middle';
      const ls = opts.series.find((s) => (s.axis || 'left') === 'left');
      if (ls) { ctx.textAlign = 'right'; ctx.fillText(ls.fmt(sc.max.left * frac), sc.l - 6, gy); }
      const rs = opts.series.find((s) => s.axis === 'right');
      if (rs) { ctx.textAlign = 'left'; ctx.fillText(rs.fmt(sc.max.right * frac), W - sc.r + 6, gy); }
    }
    // x labels
    ctx.fillStyle = muted; ctx.textBaseline = 'top'; ctx.textAlign = 'center';
    for (let i = 0; i <= 4; i++) {
      const t = sc.t0 + ((sc.t1 - sc.t0) * i) / 4;
      ctx.fillText(Math.round(t) + 's', sc.x(t), H - pad.b + 5);
    }
    if (!pts.length) {
      ctx.fillStyle = muted; ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
      ctx.fillText('Waiting for data...', W / 2, H / 2);
      return;
    }
    for (const s of opts.series) {
      ctx.strokeStyle = cssVar(s.color); ctx.lineWidth = 2; ctx.lineJoin = 'round';
      ctx.beginPath();
      pts.forEach((p, i) => {
        const px = sc.x(p.t), py = sc.y(Number(s.get(p)) || 0, s.axis);
        if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
      });
      ctx.stroke();
    }
    if (hover >= 0 && pts[hover]) {
      const p = pts[hover];
      const px = sc.x(p.t);
      ctx.strokeStyle = muted; ctx.lineWidth = 1; ctx.setLineDash([3, 3]);
      ctx.beginPath(); ctx.moveTo(px + .5, pad.t); ctx.lineTo(px + .5, H - pad.b); ctx.stroke(); ctx.setLineDash([]);
      for (const s of opts.series) {
        ctx.fillStyle = cssVar(s.color);
        ctx.beginPath(); ctx.arc(px, sc.y(Number(s.get(p)) || 0, s.axis), 3.5, 0, Math.PI * 2); ctx.fill();
      }
    }
  }

  function showTip(ev) {
    if (!pts.length) return;
    const r = canvas.getBoundingClientRect();
    const sc = scales();
    const mx = (ev.clientX - r.left);
    let best = 0, bd = Infinity;
    pts.forEach((p, i) => { const d = Math.abs(sc.x(p.t) - mx); if (d < bd) { bd = d; best = i; } });
    hover = best;
    const p = pts[best];
    tip.replaceChildren(h('strong', {}, `t = ${p.t}s`),
      ...opts.series.map((s) => h('div', {}, h('span', { style: { color: `var(${s.color})` } }, '● '), `${s.label}: ${s.fmt(s.get(p))}`)));
    tip.hidden = false;
    const tx = sc.x(p.t);
    const tw = tip.offsetWidth;
    tip.style.left = Math.max(0, Math.min(W - tw, tx + 12 + tw > W ? tx - tw - 12 : tx + 12)) + 'px';
    tip.style.top = '8px';
    draw();
  }
  function hideTip() { hover = -1; tip.hidden = true; draw(); }
  canvas.addEventListener('pointermove', showTip);
  canvas.addEventListener('pointerleave', hideTip);
  canvas.addEventListener('keydown', (ev) => {
    if (!pts.length) return;
    if (ev.key === 'ArrowLeft' || ev.key === 'ArrowRight') {
      ev.preventDefault();
      hover = Math.max(0, Math.min(pts.length - 1, (hover < 0 ? pts.length - 1 : hover) + (ev.key === 'ArrowLeft' ? -1 : 1)));
      const sc = scales(); const p = pts[hover];
      tip.replaceChildren(h('strong', {}, `t = ${p.t}s`), ...opts.series.map((s) => h('div', {}, `${s.label}: ${s.fmt(s.get(p))}`)));
      tip.hidden = false; tip.style.left = Math.min(W - tip.offsetWidth, sc.x(p.t) + 8) + 'px'; tip.style.top = '8px';
      draw();
    }
  });
  canvas.addEventListener('blur', hideTip);

  try {
    new ResizeObserver(() => draw()).observe(wrap);
  } catch { window.addEventListener('resize', draw); }
  live.add(chart);
  chart.dispose = () => live.delete(chart);
  requestAnimationFrame(draw);
  return chart;
}

export const msFmt = (v) => (v >= 1000 ? (v / 1000).toFixed(v >= 10000 ? 0 : 1) + 's' : v >= 10 ? Math.round(v) + 'ms' : (+v).toFixed(1) + 'ms');
export const numFmt = (v) => (v >= 100 ? Math.round(v).toString() : v >= 10 ? (+v).toFixed(1) : (+v).toFixed(2));
export const rateFmt = (v) => v.toFixed(v >= 10 ? 0 : 1) + '%'; // percent

export function latencyChart() {
  return createChart({ title: 'Latency percentiles', series: [
    { label: 'p50', color: '--c3', get: (p) => p.p50Ms, fmt: msFmt },
    { label: 'p95', color: '--c2', get: (p) => p.p95Ms, fmt: msFmt },
    { label: 'p99', color: '--c4', get: (p) => p.p99Ms, fmt: msFmt },
  ] });
}
export function throughputChart() {
  return createChart({ title: 'Throughput and virtual users', series: [
    { label: 'requests/s', color: '--c1', get: (p) => p.rps, fmt: numFmt },
    { label: 'active VUs', color: '--c5', axis: 'right', get: (p) => p.vus, fmt: (v) => Math.round(v).toString() },
  ] });
}
export function errorsChart() {
  return createChart({ title: 'Errors', series: [
    { label: 'errors/s', color: '--c4', get: (p) => p.errors, fmt: numFmt },
    { label: 'error rate', color: '--c2', axis: 'right', get: (p) => p.errorRate, fmt: rateFmt },
  ] });
}
