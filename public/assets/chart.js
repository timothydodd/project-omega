// Single-series area/line chart with crosshair tooltip. Plain SVG, no dependencies.
const NS = 'http://www.w3.org/2000/svg';

/** A round tick step so that 4 steps cover v (e.g. 211 -> step 60, max 240). */
function niceStep(v) {
  const raw = Math.max(v, 4) / 4;
  const exp = 10 ** Math.floor(Math.log10(raw));
  for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) if (m * exp >= raw && Number.isInteger(m * exp)) return m * exp;
  return 10 * exp;
}

function el(name, attrs = {}, parent) {
  const node = document.createElementNS(NS, name);
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  if (parent) parent.appendChild(node);
  return node;
}

/**
 * @param {HTMLElement} host
 * @param {{ points: {t:number, value:number}[], label: string, formatValue: (n:number)=>string,
 *           formatTick: (t:number)=>string, formatTitle: (t:number)=>string }} opts
 */
export function lineChart(host, opts) {
  host.innerHTML = '';
  host.style.position = 'relative';
  const svg = el('svg', { role: 'img', 'aria-label': `${opts.label} over time` }, host);
  const tip = document.createElement('div');
  tip.className = 'tooltip';
  tip.hidden = true;
  host.appendChild(tip);

  // Screen-reader / no-hover fallback: the same data as a table.
  const table = document.createElement('table');
  table.className = 'sr-only';
  table.innerHTML = `<caption>${opts.label}</caption><tr><th>Time</th><th>${opts.label}</th></tr>` +
    opts.points.map(p => `<tr><td>${opts.formatTitle(p.t)}</td><td>${opts.formatValue(p.value)}</td></tr>`).join('');
  host.appendChild(table);

  const draw = () => {
    svg.innerHTML = '';
    const W = host.clientWidth - 16, H = host.clientHeight - 24;
    if (W <= 0 || H <= 0) return;
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    const pts = opts.points;
    const step = niceStep(Math.max(0, ...pts.map(p => p.value)));
    const max = step * 4;
    const left = 44, right = 22, top = 6, bottom = 26;
    const iw = W - left - right, ih = H - top - bottom;
    const x = i => left + (pts.length <= 1 ? iw / 2 : (i / (pts.length - 1)) * iw);
    const y = v => top + ih - (v / max) * ih;

    const line = pts.map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(p.value).toFixed(1)}`).join('');
    // Area first so gridlines stay visible on top of it.
    if (pts.length) el('path', { d: `${line}L${x(pts.length - 1)},${y(0)}L${x(0)},${y(0)}Z`, class: 'area' }, svg);

    // Gridlines + y ticks (4 steps, hairline, recessive).
    for (let k = 0; k <= 4; k++) {
      const v = step * k, yy = Math.round(y(v)) + 0.5;
      el('line', { x1: left, x2: W - right, y1: yy, y2: yy, class: k === 0 ? 'axis' : 'grid' }, svg);
      const t = el('text', { x: left - 8, y: yy + 4, 'text-anchor': 'end', class: 'tick' }, svg);
      t.textContent = opts.formatValue(v);
    }
    // X ticks: at most ~7 labels.
    const every = Math.max(1, Math.ceil(pts.length / Math.max(2, Math.floor(iw / 90))));
    pts.forEach((p, i) => {
      if (i % every !== 0) return;
      const t = el('text', { x: x(i), y: H - 6, 'text-anchor': 'middle', class: 'tick' }, svg);
      t.textContent = opts.formatTick(p.t);
    });

    if (!pts.length) return;
    el('path', { d: line, class: 'line' }, svg);

    const cross = el('line', { y1: top, y2: top + ih, class: 'cross', visibility: 'hidden' }, svg);
    const dot = el('circle', { r: 5, class: 'dot', visibility: 'hidden' }, svg);
    const hit = el('rect', { x: left, y: 0, width: iw, height: H, fill: 'transparent' }, svg);

    const show = clientX => {
      const box = svg.getBoundingClientRect();
      const px = ((clientX - box.left) / box.width) * W;
      const i = Math.max(0, Math.min(pts.length - 1, Math.round(((px - left) / iw) * (pts.length - 1))));
      const p = pts[i], cx = x(i), cy = y(p.value);
      cross.setAttribute('x1', cx); cross.setAttribute('x2', cx); cross.setAttribute('visibility', 'visible');
      dot.setAttribute('cx', cx); dot.setAttribute('cy', cy); dot.setAttribute('visibility', 'visible');
      tip.innerHTML = `<div class="t-title">${opts.formatTitle(p.t)}</div><div class="t-value">${opts.formatValue(p.value)} <span class="muted" style="font-weight:400;font-size:12.5px">${opts.label.toLowerCase()}</span></div>`;
      tip.hidden = false;
      const scale = box.width / W;
      const tx = cx * scale + 12 + (box.left - host.getBoundingClientRect().left);
      const flip = tx + tip.offsetWidth > host.clientWidth;
      tip.style.left = `${flip ? tx - tip.offsetWidth - 24 : tx}px`;
      tip.style.top = `${Math.max(0, cy * scale - 20)}px`;
    };
    const hide = () => { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); dot.setAttribute('visibility', 'hidden'); };
    hit.addEventListener('pointermove', e => show(e.clientX));
    hit.addEventListener('pointerleave', hide);
  };

  draw();
  const ro = new ResizeObserver(() => draw());
  ro.observe(host);
  return () => ro.disconnect();
}
