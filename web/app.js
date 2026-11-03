// fixgw dashboard. No framework, no build step. All server-supplied text goes
// through textContent, never innerHTML: FIX fields are client-controlled.
"use strict";

const $ = (id) => document.getElementById(id);
const MAX_TAPE = 400, MAX_BLOTTER = 300, MAX_EVENTS = 60;

const state = {
  books: {}, orders: new Map(), sessions: new Map(),
  tape: [], events: [], symbol: null, seq: 0, count: 0,
};

const MSG_NAMES = {
  "0": "Heartbeat", "1": "TestRequest", "2": "ResendRequest", "3": "Reject", "4": "SequenceReset",
  "5": "Logout", "A": "Logon", "8": "ExecutionReport", "9": "OrderCancelReject", "D": "NewOrderSingle",
  "F": "OrderCancelRequest", "G": "OrderCancelReplace", "j": "BusinessReject",
};
const STATUS = { "0": "New", "1": "Partial", "2": "Filled", "4": "Canceled", "8": "Rejected" };

function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") el.className = v; else el.setAttribute(k, v);
  }
  for (const kid of kids) if (kid != null) el.append(kid);
  return el;
}

const dirty = new Set();
function schedule(name) {
  if (dirty.has(name)) return;
  dirty.add(name);
  requestAnimationFrame(() => { dirty.delete(name); renderers[name](); });
}

// ---- feed -------------------------------------------------------------

let ws, retry = 500;
function connect() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  ws = new WebSocket(`${proto}://${location.host}/ws`);
  ws.onopen = () => { retry = 500; setConn(true); };
  ws.onclose = () => { setConn(false); setTimeout(connect, retry); retry = Math.min(retry * 2, 8000); };
  ws.onerror = () => ws.close();
  ws.onmessage = (m) => {
    let ev;
    try { ev = JSON.parse(m.data); } catch { return; }
    state.count++;
    if (ev.seq) { state.seq = ev.seq; }
    handle(ev);
  };
}

function setConn(up) {
  const c = $("conn");
  c.dataset.state = up ? "up" : "down";
  c.textContent = up ? "connected" : "reconnecting";
}

function handle(ev) {
  switch (ev.kind) {
    case "snapshot": {
      const d = ev.data;
      state.books = d.books || {};
      state.orders.clear(); state.sessions.clear();
      for (const o of d.orders || []) state.orders.set(orderKey(o), o);
      for (const s of d.sessions || []) state.sessions.set(s.session_id, { id: s.session_id, on: s.logged_on, in: s.msgs_in, out: s.msgs_out });
      setHalted(d.halted);
      pickSymbol();
      schedule("book"); schedule("blotter"); schedule("sessions");
      break;
    }
    case "book":
      state.books[ev.data.symbol] = ev.data.book;
      pickSymbol();
      schedule("book");
      break;
    case "exec": applyExec(ev.data); break;
    case "cxlrej":
      addEvent(`cancel/replace rejected ${ev.data.cl_ord_id}: ${ev.data.text}`, true, ev.time);
      break;
    case "fix": addTape(ev); break;
    case "session": {
      const s = state.sessions.get(ev.data.session) || { id: ev.data.session, in: 0, out: 0 };
      s.on = ev.data.logged_on;
      state.sessions.set(s.id, s);
      addEvent(`${ev.data.session} ${s.on ? "logged on" : "logged out"}`, !s.on, ev.time);
      schedule("sessions");
      break;
    }
    case "halt":
      if (!ev.data.scope || ev.data.scope === "*") setHalted(ev.data.halted); // per-session halts do not change the global pill
      addEvent(`kill switch ${ev.data.halted ? "ON" : "off"} scope=${ev.data.scope || "all"} cancelled=${ev.data.orders_cancelled}`, ev.data.halted, ev.time);
      break;
    case "venue": {
      const d = ev.data;
      addEvent(`${d.venue} ${d.event} ${d.order_id || ""} ${d.text || d.status || (d.qty ? d.qty + " @ " + d.px : "")}`,
        d.event === "break" || d.event === "place_failed" || d.event === "cancel_failed", ev.time);
      break;
    }
  }
}

function setHalted(on) {
  const el = $("halt");
  el.dataset.state = on ? "halted" : "ok";
  el.textContent = on ? "TRADING HALTED" : "trading live";
}

// ---- state updates ----------------------------------------------------

const orderKey = (o) => (o.order_id && o.order_id !== "NONE" ? o.order_id : `${o.session}:${o.cl_ord_id}`);

function applyExec(x) {
  const k = orderKey(x);
  const prev = state.orders.get(k) || {};
  state.orders.delete(k); // re-insert so the newest activity sorts first
  state.orders.set(k, { ...prev, ...x, ord_status: x.ord_status, text: x.text });
  while (state.orders.size > MAX_BLOTTER) state.orders.delete(state.orders.keys().next().value);
  if (x.exec_type === "F") addEvent(`trade ${x.symbol} ${x.side} ${x.last_qty} @ ${x.last_px} (${x.session})`, false);
  schedule("blotter");
}

function addTape(ev) {
  if ($("pause").checked) return;
  const raw = ev.data.raw;
  const fields = raw.split("|").filter(Boolean);
  const mt = (fields.find((f) => f.startsWith("35=")) || "").slice(3);
  state.tape.push({ t: ev.time, dir: ev.data.dir, session: ev.data.session, mt, fields });
  if (state.tape.length > MAX_TAPE) state.tape.shift();
  schedule("tape");
}

function addEvent(text, bad, time) {
  state.events.unshift({ text, bad, t: time || new Date().toISOString() });
  if (state.events.length > MAX_EVENTS) state.events.pop();
  schedule("events");
}

function pickSymbol() {
  const syms = Object.keys(state.books).sort();
  const sel = $("symbol");
  if (sel.options.length !== syms.length) {
    sel.replaceChildren(...syms.map((s) => h("option", { value: s }, s)));
  }
  if (!state.symbol || !syms.includes(state.symbol)) state.symbol = syms[0] || null;
  sel.value = state.symbol || "";
}

// ---- rendering --------------------------------------------------------

// fmt renders a fixed-point integer with the given decimals.
function fmt(v, d) {
  const neg = v < 0; let s = String(Math.abs(v));
  if (d > 0) { s = s.padStart(d + 1, "0"); s = s.slice(0, -d) + "." + s.slice(-d); }
  return (neg ? "-" : "") + s;
}
const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour12: false });
// "FIX.4.4:FIXGW->CLIENT1" is noise on a page that only ever shows our own sessions.
const shortSession = (s) => (s || "").replace(/^.*->/, "");

const renderers = {
  book() {
    const root = $("ladder"), b = state.books[state.symbol];
    const routed = $("routed");
    routed.hidden = !(b && b.routed);
    routed.textContent = b && b.routed ? `routed to ${b.routed}` : "";
    if (!b) { root.replaceChildren(h("div", { class: "empty" }, "no book")); return; }
    const pd = b.price_decimals, qd = b.qty_decimals;
    const levels = [...b.asks, ...b.bids];
    const max = Math.max(1, ...levels.map((l) => l.qty));
    const row = (cls, l) => h("div", { class: `row ${cls}` },
      h("div", { class: "bar", style: `width:${Math.max(2, Math.round((l.qty / max) * 100))}%` }),
      h("span", { class: "px" }, fmt(l.price, pd)), h("span", {}, fmt(l.qty, qd)), h("span", {}, String(l.orders)));
    const kids = [h("div", { class: "row head" }, h("span", {}, "price"), h("span", {}, "qty"), h("span", {}, "#"))];
    [...b.asks].reverse().forEach((l) => kids.push(row("ask", l)));
    const spread = b.asks.length && b.bids.length ? fmt(b.asks[0].price - b.bids[0].price, pd) : "–";
    kids.push(h("div", { class: "spread" }, `spread ${spread}`));
    b.bids.forEach((l) => kids.push(row("bid", l)));
    if (!levels.length) kids.push(h("div", { class: "empty" }, b.routed ? "orders for this symbol trade at the venue" : "book is empty"));
    root.replaceChildren(...kids);
  },

  tape() {
    const root = $("tape");
    const stick = root.scrollTop + root.clientHeight >= root.scrollHeight - 30;
    const hide = $("hide-hb").checked;
    const rows = state.tape.filter((m) => !(hide && m.mt === "0")).slice(-200).map((m) => {
      const body = h("span", {});
      m.fields.forEach((f, i) => {
        const [k, ...v] = f.split("=");
        if (i) body.append(" ");
        body.append(h("span", { class: "k" }, k + "="), v.join("="));
      });
      return h("div", { class: `msg ${m.dir}` },
        h("span", { class: "t" }, clock(m.t)), h("span", { class: "d" }, m.dir === "in" ? "←" : "→"),
        h("span", { class: "n" }, MSG_NAMES[m.mt] || m.mt), body);
    });
    root.replaceChildren(...rows);
    if (stick) root.scrollTop = root.scrollHeight;
  },

  blotter() {
    const tbody = $("blotter").tBodies[0];
    const rows = [...state.orders.values()].reverse().map((o) => h("tr", {},
      h("td", {}, o.cl_ord_id), h("td", {}, shortSession(o.session)), h("td", {}, o.symbol),
      h("td", { class: o.side === "BUY" ? "buy" : "sell" }, o.side),
      h("td", { class: "num" }, o.qty), h("td", { class: "num" }, o.price),
      h("td", { class: "num" }, o.cum_qty), h("td", { class: "num" }, o.avg_px),
      h("td", { class: `st-${o.ord_status}`, title: o.text || "" }, (STATUS[o.ord_status] || o.ord_status) + (o.text ? " ⓘ" : ""))));
    tbody.replaceChildren(...rows);
    $("blotter-count").textContent = `${state.orders.size} orders`;
  },

  sessions() {
    $("sessions").replaceChildren(...[...state.sessions.values()].sort((a, b) => a.id.localeCompare(b.id)).map((s) =>
      h("div", { class: `sess${s.on ? " on" : ""}` }, h("b", {}, h("span", { class: "dot" }), shortSession(s.id)),
        h("div", { class: "muted" }, s.on ? "logged on" : "offline"))));
  },

  events() {
    $("events").replaceChildren(...state.events.map((e) =>
      h("li", { class: e.bad ? "bad" : "" }, `${clock(e.t)}  ${e.text}`)));
  },
};

// ---- controls ---------------------------------------------------------

$("symbol").addEventListener("change", (e) => { state.symbol = e.target.value; schedule("book"); });
$("hide-hb").addEventListener("change", () => schedule("tape"));

async function killSwitch(on) {
  const msg = $("ks-msg");
  const token = $("ks-token").value;
  if (!token) { msg.textContent = "enter the admin token"; return; }
  const q = new URLSearchParams({ on: String(on), scope: $("ks-scope").value.trim(), mass_cancel: String($("ks-mass").checked) });
  msg.textContent = "…";
  try {
    const r = await fetch(`/api/killswitch?${q}`, { method: "POST", headers: { Authorization: `Bearer ${token}` } });
    if (r.status === 403) msg.textContent = "forbidden: wrong token, or the endpoint is disabled";
    else if (!r.ok) msg.textContent = `error ${r.status}`;
    else { const j = await r.json(); msg.textContent = on ? `halted, ${j.orders_cancelled} orders cancelled` : "resumed"; }
  } catch { msg.textContent = "request failed"; }
}
$("kill").addEventListener("submit", (e) => { e.preventDefault(); killSwitch(true); });
$("ks-resume").addEventListener("click", () => killSwitch(false));

// Events per second, shown in the ops strip.
let lastCount = 0;
setInterval(() => {
  $("rate").textContent = String(state.count - lastCount); lastCount = state.count;
  $("seq").textContent = String(state.seq);
}, 1000);

connect();
