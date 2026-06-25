import { useState, useEffect, useCallback, useRef, useMemo, createContext, useContext, forwardRef } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

// ═══════════════════════════════════════════════════════════════════
// Types
// ═══════════════════════════════════════════════════════════════════

interface AnglStatus {
  name: string; state: string; enabled: boolean; pid?: number;
  started?: string; uptime?: string; last_exit?: string;
  next_run?: string; next_run_in?: string; restarts: number;
  max_restarts?: number; interval?: string; created_at?: string;
  lifetime?: string; charge?: string; tags?: string[];
  endpoint?: { http?: string };
}

interface TaskView {
  id: string; title: string; description?: string; priority: number;
  submitted?: string; attempts?: number; cancels?: number;
  reason?: string; leasedAt?: string; caller?: string;
  kv?: Record<string, string>;
}

interface DepEdge { from: string; to: string; }

interface QueueSnapshot {
  name: string; driver: string;
  ready: TaskView[]; blocked: TaskView[]; inflight: TaskView[];
  dead: TaskView[]; completed: TaskView[];
  deps: DepEdge[]; blockedBy: Record<string, string[]>;
  counts: Record<string, number>; dbMeta?: Record<string, string>;
  snapshotAt: string;
}

interface QueueEntry { name: string; driver: string; path: string; comparator: string; }
type Theme = "crimson" | "azure";
type ListTab = "ready" | "blocked" | "inflight" | "dead" | "completed";

type ViewType =
  | { kind: "angl-list" }
  | { kind: "schedg-list" }
  | { kind: "angl-detail"; name: string }
  | { kind: "schedg-detail"; queue: string }
  | { kind: "conversation"; orchardUrl: string; envId: string; convId: string; anglName?: string };

type PaneNode =
  | { type: "leaf"; id: string; view: ViewType }
  | { type: "split"; id: string; direction: "h" | "v"; ratio: number; a: PaneNode; b: PaneNode };

// ═══════════════════════════════════════════════════════════════════
// Global Context
// ═══════════════════════════════════════════════════════════════════

interface GlobalData {
  angls: AnglStatus[];
  queues: QueueEntry[];
  theme: Theme;
  focusedPaneId: string;
  setFocusedPaneId: (id: string) => void;
  splitPane: (id: string, dir: "h" | "v") => void;
  closePane: (id: string) => void;
  setView: (id: string, view: ViewType) => void;
  openIn: (id: string, view: ViewType, mode: "current" | "split-h" | "split-v") => void;
  execCommand: (cmd: string) => string | null;
  dispatchTask: (queue: string, taskId: string) => void;
}

const DataCtx = createContext<GlobalData>(null!);

// ═══════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════

let nextPaneId = 1;
function genId() { return `p${nextPaneId++}`; }

function relativeTime(iso: string): string {
  if (!iso) return "";
  const d = Date.now() - new Date(iso).getTime();
  if (d < 0) return "now";
  if (d < 60000) return `${Math.floor(d / 1000)}s ago`;
  if (d < 3600000) return `${Math.floor(d / 60000)}m ago`;
  if (d < 86400000) return `${Math.floor(d / 3600000)}h ago`;
  return `${Math.floor(d / 86400000)}d ago`;
}

function copyText(text: string) { navigator.clipboard.writeText(text); }
function fuzzyMatch(q: string, t: string): boolean {
  if (!q) return true;
  const ql = q.toLowerCase(), tl = t.toLowerCase();
  let qi = 0;
  for (let i = 0; i < tl.length && qi < ql.length; i++) if (tl[i] === ql[qi]) qi++;
  return qi === ql.length;
}
function stateColor(s: string): string {
  switch (s) {
    case "running": return "var(--green-bright)";
    case "starting": case "backoff": return "var(--gold)";
    case "failed": return "var(--accent-glow)";
    default: return "var(--text-3)";
  }
}

// ── Conversation types (must be before any component that uses them) ──

interface ConvTurn {
  type: "turn";
  user: string;
  content: string[];
  toolCalls: Record<string, { name?: string; args?: string; result?: string; error?: string; status?: string }>;
  status: "Streaming" | "Complete" | "Failed";
  timestamp: string;
  thinkingContent?: string[];
}

interface ConvRead {
  startIndex: number;
  version: number;
  messages: ConvTurn[];
}

// ── Shared components (must be before any component that uses them) ──

const SearchBar = forwardRef<HTMLInputElement, {value:string;onChange:(v:string)=>void;count:number;placeholder:string}>(
  ({value,onChange,count,placeholder},ref) => (
    <div className="search-row">
      <input ref={ref} className="search" type="text" placeholder={placeholder} value={value} onChange={e=>onChange(e.target.value)} spellCheck={false} />
      <span className="search-count">{count}</span>
    </div>
  )
);

function CopyBtn({text,className}:{text:string;className?:string}) {
  return <button className={`copy-btn ${className??""}`} title="Copy" onClick={e=>{e.stopPropagation();copyText(text);}}>&#x2398;</button>;
}

function MetaRow({k,v,accent}:{k:string;v:string;accent?:boolean}) {
  return <div className="meta-row"><span className="meta-key">{k}</span><span className={`meta-val ${accent?"meta-accent":""}`}>{v}</span></div>;
}

function useSSE<T>(url: string | null, onMessage: (data: T) => void) {
  useEffect(() => {
    if (!url) return;
    const es = new EventSource(url);
    es.onmessage = (e) => { try { onMessage(JSON.parse(e.data)); } catch {} };
    es.onerror = () => { es.close(); };
    return () => es.close();
  }, [url]); // eslint-disable-line
}

// ═══════════════════════════════════════════════════════════════════
// Pane tree ops
// ═══════════════════════════════════════════════════════════════════

function countLeaves(n: PaneNode): number { return n.type === "leaf" ? 1 : countLeaves(n.a) + countLeaves(n.b); }
function findLeaf(n: PaneNode, id: string): (PaneNode & {type:"leaf"}) | null {
  if (n.type === "leaf") return n.id === id ? n : null;
  return findLeaf(n.a, id) || findLeaf(n.b, id);
}
function allLeafIds(n: PaneNode): string[] { return n.type === "leaf" ? [n.id] : [...allLeafIds(n.a), ...allLeafIds(n.b)]; }
function mapLeaf(n: PaneNode, id: string, fn: (l: PaneNode & {type:"leaf"}) => PaneNode): PaneNode {
  if (n.type === "leaf") return n.id === id ? fn(n) : n;
  return { ...n, a: mapLeaf(n.a, id, fn), b: mapLeaf(n.b, id, fn) };
}
function splitLeaf(tree: PaneNode, leafId: string, dir: "h" | "v"): PaneNode {
  if (countLeaves(tree) >= 8) return tree;
  return mapLeaf(tree, leafId, (leaf) => ({ type: "split", id: genId(), direction: dir, ratio: 0.5, a: leaf, b: { type: "leaf", id: genId(), view: { kind: "angl-list" } } }));
}
function removeLeaf(tree: PaneNode, leafId: string): PaneNode | null {
  if (tree.type === "leaf") return tree.id === leafId ? null : tree;
  const a = removeLeaf(tree.a, leafId), b = removeLeaf(tree.b, leafId);
  if (!a) return b; if (!b) return a;
  return { ...tree, a, b };
}
function updateRatio(tree: PaneNode, splitId: string, ratio: number): PaneNode {
  if (tree.type === "leaf") return tree;
  if (tree.id === splitId) return { ...tree, ratio: Math.max(0.1, Math.min(0.9, ratio)) };
  return { ...tree, a: updateRatio(tree.a, splitId, ratio), b: updateRatio(tree.b, splitId, ratio) };
}

// ── URL serialization ────────────────────────────────────────────
// Format: recursive, compact
//   leaf:  al | sl | ad:<name> | sd:<queue>
//   split: h<ratio>(<a>,<b>) | v<ratio>(<a>,<b>)
//   ratio: 2-digit integer (50 = 0.50)

function serializeView(v: ViewType): string {
  switch (v.kind) {
    case "angl-list": return "al";
    case "schedg-list": return "sl";
    case "angl-detail": return "ad:" + encodeURIComponent(v.name);
    case "schedg-detail": return "sd:" + encodeURIComponent(v.queue);
    case "conversation": return "cv:" + [v.orchardUrl, v.envId, v.convId].map(encodeURIComponent).join(",");
  }
}

function serializeTree(n: PaneNode): string {
  if (n.type === "leaf") return serializeView(n.view);
  const r = Math.round(n.ratio * 100);
  return `${n.direction}${r}(${serializeTree(n.a)},${serializeTree(n.b)})`;
}

function deserializeView(s: string): ViewType {
  if (s === "al") return { kind: "angl-list" };
  if (s === "sl") return { kind: "schedg-list" };
  if (s.startsWith("ad:")) return { kind: "angl-detail", name: decodeURIComponent(s.slice(3)) };
  if (s.startsWith("sd:")) return { kind: "schedg-detail", queue: decodeURIComponent(s.slice(3)) };
  if (s.startsWith("cv:")) { const [url,env,conv] = s.slice(3).split(",").map(decodeURIComponent); return { kind: "conversation", orchardUrl: url, envId: env, convId: conv }; }
  return { kind: "angl-list" };
}

function deserializeTree(s: string): PaneNode {
  // Try to parse as split: h50(...,...) or v50(...)
  const m = s.match(/^([hv])(\d+)\((.+)\)$/);
  if (m) {
    const dir = m[1] as "h" | "v";
    const ratio = parseInt(m[2]) / 100;
    // Find the comma that splits a and b at the top level (respecting nested parens)
    const inner = m[3];
    let depth = 0, splitAt = -1;
    for (let i = 0; i < inner.length; i++) {
      if (inner[i] === "(") depth++;
      else if (inner[i] === ")") depth--;
      else if (inner[i] === "," && depth === 0) { splitAt = i; break; }
    }
    if (splitAt >= 0) {
      return {
        type: "split", id: genId(), direction: dir, ratio,
        a: deserializeTree(inner.slice(0, splitAt)),
        b: deserializeTree(inner.slice(splitAt + 1)),
      };
    }
  }
  // Leaf
  return { type: "leaf", id: genId(), view: deserializeView(s) };
}

// ═══════════════════════════════════════════════════════════════════
// Command system
// ═══════════════════════════════════════════════════════════════════

interface CmdDef {
  name: string;
  aliases?: string[];
  args?: string;
  desc: string;
  completionCtx?: string;
}

interface CompletionItem {
  value: string;
  label: string;
  detail?: string;
}

const COMMANDS: CmdDef[] = [
  { name: "split", args: "h|v", desc: "Split focused pane", completionCtx: "directions" },
  { name: "vsplit", desc: "Split right" },
  { name: "hsplit", desc: "Split below" },
  { name: "close", aliases: ["q"], desc: "Close focused pane" },
  { name: "view", args: "angls|queues", desc: "Switch pane view", completionCtx: "views" },
  { name: "open", args: "<name>", desc: "Open angl or queue", completionCtx: "open" },
  { name: "queue", args: "<name>", desc: "Open a queue", completionCtx: "queues" },
  { name: "theme", args: "crimson|azure", desc: "Switch theme", completionCtx: "themes" },
  { name: "resize", desc: "Resize mode (hjkl, Esc)" },
  { name: "swap", desc: "Swap mode (hjkl, Esc)" },
  { name: "focus", args: "h|j|k|l|next|prev", desc: "Move focus", completionCtx: "directions" },
  { name: "start", args: "<angl>", desc: "Start an angl", completionCtx: "angls" },
  { name: "stop", args: "<angl>", desc: "Stop an angl", completionCtx: "angls" },
  { name: "enable", args: "<angl>", desc: "Enable an angl", completionCtx: "angls" },
  { name: "disable", args: "<angl>", desc: "Disable an angl", completionCtx: "angls" },
  { name: "restart", args: "<angl>", desc: "Restart an angl", completionCtx: "angls" },
  { name: "message", args: "<angl> <text>", desc: "Send message to an angl", completionCtx: "angls" },
  { name: "status", args: "<angl>", desc: "Show angl status", completionCtx: "angls" },
  { name: "dispatch", args: "<queue> <taskId>", desc: "Create an author angl for a schedg task" },
  { name: "conversation", aliases: ["conv"], args: "<convId>", desc: "Open conversation view (local orchard)" },
  { name: "create", args: "<name> [--interval <dur>] [--charge <desc>]", desc: "Create a new angl" },
  { name: "create-chat", args: "<name>", desc: "Create a new orchard conversation agent" },
  { name: "complete", args: "<queue> <id>", desc: "Complete an in-flight task" },
  { name: "cancel", args: "<queue> <id>", desc: "Return in-flight task to ready" },
  { name: "fail", args: "<queue> <id>", desc: "Bury a task to dead-letter" },
  { name: "requeue", args: "<queue> <id>", desc: "Kick a dead task back to ready" },
  { name: "add", args: "<queue> <title>", desc: "Add a task to a queue" },
  { name: "help", aliases: ["?"], desc: "Show commands" },
];

// ═══════════════════════════════════════════════════════════════════
// App
// ═══════════════════════════════════════════════════════════════════

export function App() {
  const [theme, setTheme] = useState<Theme>(() => (localStorage.getItem("angl-theme") as Theme) || "crimson");
  useEffect(() => { document.documentElement.setAttribute("data-theme", theme); }, [theme]);

  const [angls, setAngls] = useState<AnglStatus[]>([]);
  const [queues, setQueues] = useState<QueueEntry[]>([]);
  useSSE<AnglStatus[]>("/api/angls/events", setAngls);
  useEffect(() => { fetch("/api/queues").then(r => r.json()).then(setQueues).catch(() => {}); }, []);

  const [tree, setTree] = useState<PaneNode>(() => {
    const hash = window.location.hash.slice(1);
    if (hash) {
      try { return deserializeTree(decodeURIComponent(hash)); } catch {}
    }
    return { type: "leaf", id: genId(), view: { kind: "angl-list" } };
  });
  const [focusedPaneId, setFocusedPaneId] = useState(() => allLeafIds(tree)[0]);

  // Sync tree to URL hash (pushState so back button works)
  const treeRef = useRef(tree);
  useEffect(() => {
    const serialized = serializeTree(tree);
    const current = window.location.hash.slice(1);
    if (serialized !== current) {
      // Only push if tree actually changed (not on initial mount)
      if (treeRef.current !== tree) {
        window.history.pushState(null, "", "#" + serialized);
      } else {
        window.history.replaceState(null, "", "#" + serialized);
      }
      treeRef.current = tree;
    }
  }, [tree]);

  // Handle browser back/forward
  useEffect(() => {
    const onPop = () => {
      const hash = window.location.hash.slice(1);
      if (hash) {
        try {
          const restored = deserializeTree(decodeURIComponent(hash));
          treeRef.current = restored;
          setTree(restored);
          setFocusedPaneId(allLeafIds(restored)[0]);
        } catch {}
      }
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const [mode, setMode] = useState<"normal" | "resize" | "swap" | "command">("normal");
  const [cmdInput, setCmdInput] = useState("");
  const [cmdSelectedIdx, setCmdSelectedIdx] = useState(0);
  const cmdRef = useRef<HTMLInputElement>(null);

  const splitPane = useCallback((id: string, dir: "h" | "v") => setTree(t => splitLeaf(t, id, dir)), []);
  const closePane = useCallback((id: string) => {
    setTree(t => { const nt = removeLeaf(t, id); if (!nt) return t; setFocusedPaneId(fid => allLeafIds(nt).includes(fid) ? fid : allLeafIds(nt)[0]); return nt; });
  }, []);
  const setPaneView = useCallback((id: string, view: ViewType) => setTree(t => mapLeaf(t, id, l => ({ ...l, view }))), []);
  const openIn = useCallback((id: string, view: ViewType, m: "current" | "split-h" | "split-v") => {
    if (m === "current") { setPaneView(id, view); return; }
    setTree(t => {
      if (countLeaves(t) >= 8) { setPaneView(id, view); return t; }
      const dir = m === "split-h" ? "h" : "v";
      return mapLeaf(t, id, leaf => {
        const nid = genId(); setFocusedPaneId(nid);
        return { type: "split", id: genId(), direction: dir, ratio: 0.5, a: leaf, b: { type: "leaf", id: nid, view } };
      });
    });
  }, [setPaneView]);

  // Spatial nav
  const navigateDir = useCallback((dir: "h"|"j"|"k"|"l") => {
    const el = document.querySelector(`[data-pane-id="${focusedPaneId}"]`);
    if (!el) return;
    const fr = el.getBoundingClientRect(), cx = fr.left+fr.width/2, cy = fr.top+fr.height/2;
    let best: string|null = null, bestD = Infinity;
    for (const id of allLeafIds(tree)) {
      if (id === focusedPaneId) continue;
      const e2 = document.querySelector(`[data-pane-id="${id}"]`);
      if (!e2) continue;
      const r = e2.getBoundingClientRect(), dx = r.left+r.width/2-cx, dy = r.top+r.height/2-cy;
      let ok = false;
      if (dir==="h"&&dx<-20) ok=true; if (dir==="l"&&dx>20) ok=true;
      if (dir==="k"&&dy<-20) ok=true; if (dir==="j"&&dy>20) ok=true;
      if (ok) { const d=Math.abs(dx)+Math.abs(dy); if (d<bestD){bestD=d;best=id;} }
    }
    if (best) setFocusedPaneId(best);
  }, [focusedPaneId, tree]);

  const resizeDir = useCallback((dir: "h"|"j"|"k"|"l") => {
    setTree(t => {
      const adj = (node: PaneNode, childId: string): PaneNode => {
        if (node.type === "leaf") return node;
        const inA = !!findLeaf(node.a, childId), inB = !!findLeaf(node.b, childId);
        if ((inA||inB) && ((node.direction==="v"&&(dir==="h"||dir==="l")) || (node.direction==="h"&&(dir==="k"||dir==="j")))) {
          const sign = (dir==="l"||dir==="j") ? 1 : -1;
          const d = inA ? sign : -sign;
          return { ...node, ratio: Math.max(0.1, Math.min(0.9, node.ratio + d * 0.05)) };
        }
        return { ...node, a: adj(node.a, childId), b: adj(node.b, childId) };
      };
      return adj(t, focusedPaneId);
    });
  }, [focusedPaneId]);

  const swapWith = useCallback((dir: "h"|"j"|"k"|"l") => {
    const el = document.querySelector(`[data-pane-id="${focusedPaneId}"]`);
    if (!el) return;
    const fr = el.getBoundingClientRect(), cx=fr.left+fr.width/2, cy=fr.top+fr.height/2;
    let best: string|null=null, bestD=Infinity;
    for (const id of allLeafIds(tree)) {
      if (id===focusedPaneId) continue;
      const e2=document.querySelector(`[data-pane-id="${id}"]`);
      if(!e2) continue;
      const r=e2.getBoundingClientRect(), dx=r.left+r.width/2-cx, dy=r.top+r.height/2-cy;
      let ok=false;
      if(dir==="h"&&dx<-20)ok=true;if(dir==="l"&&dx>20)ok=true;
      if(dir==="k"&&dy<-20)ok=true;if(dir==="j"&&dy>20)ok=true;
      if(ok){const d=Math.abs(dx)+Math.abs(dy);if(d<bestD){bestD=d;best=id;}}
    }
    if (best) {
      setTree(t => {
        const s = findLeaf(t, focusedPaneId), d2 = findLeaf(t, best!);
        if (!s||!d2) return t;
        let nt = mapLeaf(t, focusedPaneId, l => ({...l, view: d2.view}));
        nt = mapLeaf(nt, best!, l => ({...l, view: s.view}));
        return nt;
      });
    }
    setMode("normal");
  }, [focusedPaneId, tree]);

  // Command execution
  const execCommand = useCallback((raw: string): string | null => {
    const parts = raw.trim().split(/\s+/);
    const cmd = parts[0]?.toLowerCase();
    const arg = parts.slice(1).join(" ");

    if (!cmd) return null;
    if (cmd === "split" || cmd === "sp") {
      const dir = arg === "h" || arg === "horizontal" ? "h" : "v";
      splitPane(focusedPaneId, dir); return null;
    }
    if (cmd === "vsplit" || cmd === "vs") { splitPane(focusedPaneId, "v"); return null; }
    if (cmd === "hsplit" || cmd === "hs") { splitPane(focusedPaneId, "h"); return null; }
    if (cmd === "close" || cmd === "q") { closePane(focusedPaneId); return null; }
    if (cmd === "view") {
      if (arg === "angls" || arg === "angl-list") { setPaneView(focusedPaneId, {kind:"angl-list"}); return null; }
      if (arg === "queues" || arg === "schedg-list") { setPaneView(focusedPaneId, {kind:"schedg-list"}); return null; }
      return `Unknown view: ${arg}`;
    }
    if (cmd === "open" || cmd === "o") {
      if (!arg) return "Usage: open <angl-name|queue-name>";
      const anglMatch = angls.find(a => a.name === arg || fuzzyMatch(arg, a.name));
      if (anglMatch) { setPaneView(focusedPaneId, {kind:"angl-detail", name: anglMatch.name}); return null; }
      const queueMatch = queues.find(q => q.name === arg || fuzzyMatch(arg, q.name));
      if (queueMatch) { setPaneView(focusedPaneId, {kind:"schedg-detail", queue: queueMatch.name}); return null; }
      return `Not found: ${arg}`;
    }
    if (cmd === "queue" || cmd === "qu") {
      if (!arg) return "Usage: queue <name>";
      setPaneView(focusedPaneId, {kind:"schedg-detail", queue: arg}); return null;
    }
    if (cmd === "theme") {
      if (arg === "crimson" || arg === "azure") { setTheme(arg); localStorage.setItem("angl-theme", arg); return null; }
      setTheme(t => { const n = t === "crimson" ? "azure" : "crimson"; localStorage.setItem("angl-theme", n); return n; }); return null;
    }
    if (cmd === "resize") { setMode("resize"); return null; }
    if (cmd === "swap") { setMode("swap"); return null; }
    if (cmd === "focus") {
      if ("hjkl".includes(arg)) { navigateDir(arg as any); return null; }
      if (arg === "next") { const ls = allLeafIds(tree); setFocusedPaneId(ls[(ls.indexOf(focusedPaneId)+1)%ls.length]); return null; }
      if (arg === "prev") { const ls = allLeafIds(tree); setFocusedPaneId(ls[(ls.indexOf(focusedPaneId)-1+ls.length)%ls.length]); return null; }
      return `Usage: focus h|j|k|l|next|prev`;
    }
    // CLI proxy commands
    if (["start","stop","enable","disable","restart"].includes(cmd)) {
      if (!arg) return `Usage: ${cmd} <angl-name>`;
      fetch("/api/rpc", { method: "POST", headers: {"Content-Type":"application/json"}, body: JSON.stringify({jsonrpc:"2.0",method:cmd,params:{name:arg},id:1}) })
        .then(r => r.json()).then(d => { if (d.error) setCmdError(d.error.message); }).catch(e => setCmdError(String(e)));
      return null;
    }
    if (cmd === "message" || cmd === "msg") {
      const spaceIdx = arg.indexOf(" ");
      if (spaceIdx < 0) return "Usage: message <angl-name> <text>";
      const name = arg.slice(0, spaceIdx), text = arg.slice(spaceIdx + 1);
      fetch("/api/rpc", { method: "POST", headers: {"Content-Type":"application/json"}, body: JSON.stringify({jsonrpc:"2.0",method:"message",params:{name,prompt:text,from:"web"},id:1}) })
        .then(r => r.json()).then(d => { if (d.error) setCmdError(d.error.message); }).catch(e => setCmdError(String(e)));
      return null;
    }
    if (cmd === "status") {
      if (!arg) return "Usage: status <angl-name>";
      setPaneView(focusedPaneId, {kind:"angl-detail", name: arg}); return null;
    }
    if (cmd === "create-chat") {
      if (!arg) return "Usage: create-chat <name>";
      fetch("/api/rpc", { method: "POST", headers: {"Content-Type":"application/json"},
        body: JSON.stringify({jsonrpc:"2.0",method:"create-chat",params:{name:arg},id:1})
      }).then(r => r.json()).then(d => {
        if (d.error) { setCmdError(d.error.message); return; }
        const name = d.result?.name;
        if (name) setPaneView(focusedPaneId, {kind:"angl-detail", name});
      }).catch(e => setCmdError(String(e)));
      return null;
    }
    if (cmd === "conversation" || cmd === "conv") {
      if (!arg) return "Usage: conversation <conversationId>";
      setPaneView(focusedPaneId, {
        kind: "conversation",
        orchardUrl: "https://forintracommuseonly.localhost:50772",
        envId: "56c322d4-942e-e277-ab14-99f4a5a4f3ab",
        convId: arg,
      });
      return null;
    }
    if (cmd === "dispatch") {
      const parts2 = arg.split(/\s+/);
      if (parts2.length < 2) return "Usage: dispatch <queue> <taskId>";
      dispatchTask(parts2[0], parts2[1]);
      return null;
    }
    // Schedg CRUD commands
    if (["complete","cancel","fail","requeue"].includes(cmd)) {
      const parts2 = arg.split(/\s+/);
      if (parts2.length < 2) return `Usage: ${cmd} <queue> <id>`;
      const [q, id] = parts2;
      schedgRpc(`schedg-${cmd}`, { queue: q, id }).catch(e => setCmdError(String(e)));
      return null;
    }
    if (cmd === "add") {
      const spIdx = arg.indexOf(" ");
      if (spIdx < 0) return "Usage: add <queue> <title>";
      const q = arg.slice(0, spIdx), title = arg.slice(spIdx + 1);
      schedgRpc("schedg-add", { queue: q, title, description: "", priority: 0 }).catch(e => setCmdError(String(e)));
      return null;
    }
    if (cmd === "help" || cmd === "?") { return COMMANDS.map(c => `${c.name}${c.args?` ${c.args}`:""} -- ${c.desc}`).join("\n"); }
    return `Unknown command: ${cmd}`;
  }, [angls, queues, focusedPaneId, splitPane, closePane, setPaneView, navigateDir, tree]);

  // Hotkeys
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const inInput = (e.target as HTMLElement)?.tagName === "INPUT" || (e.target as HTMLElement)?.tagName === "TEXTAREA";

      // Command mode toggle
      if (e.key === ":" && !inInput && mode !== "command") {
        e.preventDefault(); setMode("command"); setCmdInput(""); setCmdSelectedIdx(0);
        setTimeout(() => cmdRef.current?.focus(), 0);
        return;
      }

      if (mode === "command") return; // command input handles its own keys
      if (inInput) return;

      if (e.key === "Escape") { setMode("normal"); return; }

      if (mode === "resize") {
        if ("hjkl".includes(e.key)) { e.preventDefault(); resizeDir(e.key as any); return; }
        return;
      }
      if (mode === "swap") {
        if ("hjkl".includes(e.key)) { e.preventDefault(); swapWith(e.key as any); return; }
        return;
      }

      // Mode toggles
      if (e.key === "r" && e.ctrlKey) { e.preventDefault(); setMode(m => m === "resize" ? "normal" : "resize"); return; }
      if (e.key === "x" && e.ctrlKey) { e.preventDefault(); setMode(m => m === "swap" ? "normal" : "swap"); return; }

      // Normal mode
      if (e.key === "-" && !e.ctrlKey) { e.preventDefault(); splitPane(focusedPaneId, "h"); return; }
      if (e.key === "|" || (e.key === "\\" && e.shiftKey)) { e.preventDefault(); splitPane(focusedPaneId, "v"); return; }
      if (e.key === "w" && e.ctrlKey) { e.preventDefault(); closePane(focusedPaneId); return; }
      if (e.ctrlKey && "hjkl".includes(e.key)) { e.preventDefault(); navigateDir(e.key as any); return; }
      if (e.key === "Tab") {
        e.preventDefault();
        const ls = allLeafIds(tree);
        setFocusedPaneId(ls[(ls.indexOf(focusedPaneId) + (e.shiftKey?-1:1) + ls.length) % ls.length]);
        return;
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [focusedPaneId, tree, mode, splitPane, closePane, navigateDir, resizeDir, swapWith]);

  // Context menu
  const [ctxMenu, setCtxMenu] = useState<{x:number;y:number;paneId:string}|null>(null);
  useEffect(() => {
    const onCtx = (e: MouseEvent) => {
      const pane = (e.target as HTMLElement).closest("[data-pane-id]");
      if (!pane) return;
      e.preventDefault();
      setFocusedPaneId(pane.getAttribute("data-pane-id")!);
      setCtxMenu({x:e.clientX, y:e.clientY, paneId:pane.getAttribute("data-pane-id")!});
    };
    const dismiss = () => setCtxMenu(null);
    window.addEventListener("contextmenu", onCtx);
    window.addEventListener("click", dismiss);
    return () => { window.removeEventListener("contextmenu", onCtx); window.removeEventListener("click", dismiss); };
  }, []);

  const dispatchTask = useCallback((queue: string, taskId: string) => {
    fetch("/api/rpc", {
      method: "POST", headers: {"Content-Type":"application/json"},
      body: JSON.stringify({jsonrpc:"2.0",method:"dispatch",params:{queue,task_id:taskId},id:1})
    }).then(r => r.json()).then(d => {
      if (d.error) { setCmdError(d.error.message); return; }
      const name = d.result?.name;
      if (name) {
        // Open the new angl in a split
        openIn(focusedPaneId, {kind:"angl-detail", name}, "split-v");
      }
    }).catch(e => setCmdError(String(e)));
  }, [focusedPaneId, openIn]);

  const ctx: GlobalData = { angls, queues, theme, focusedPaneId, setFocusedPaneId, splitPane, closePane, setView: setPaneView, openIn, execCommand, dispatchTask };

  // Cached completions from API
  const [apiCompletions, setApiCompletions] = useState<Record<string, CompletionItem[]>>({});
  useEffect(() => {
    if (mode !== "command") return;
    const contexts = ["angls", "queues", "themes", "views", "directions"];
    for (const ctx of contexts) {
      if (apiCompletions[ctx]) continue;
      fetch(`/api/completions?context=${ctx}`).then(r => r.json()).then(data => {
        setApiCompletions(prev => ({ ...prev, [ctx]: data }));
      }).catch(() => {});
    }
  }, [mode]); // eslint-disable-line

  const cmdMatches = useMemo(() => {
    if (mode !== "command") return [];
    const raw = cmdInput.toLowerCase();
    const hasTrailingSpace = raw.endsWith(" ");
    const trimmed = raw.trim();
    const parts = trimmed.split(/\s+/);
    const cmd = parts[0] || "";
    const arg = parts.slice(1).join(" ");
    const inArgPosition = parts.length > 1 || (parts.length === 1 && hasTrailingSpace);

    // Command name completion (no space typed yet)
    if (!inArgPosition) {
      return COMMANDS.filter(c => !cmd || c.name.startsWith(cmd) || (c.aliases ?? []).some(a => a.startsWith(cmd)))
        .map(c => ({ label: c.name, detail: c.desc, value: c.name + (c.args ? " " : "") }));
    }

    // Find the matched command
    const cmdDef = COMMANDS.find(c => c.name === cmd || (c.aliases ?? []).includes(cmd));
    if (!cmdDef) return [];

    // Special case: "open" searches both angls and queues
    if (cmd === "open" || cmd === "o") {
      const items: {label:string;detail:string;value:string}[] = [];
      for (const a of (apiCompletions.angls ?? [])) if (!arg || fuzzyMatch(arg, a.label)) items.push({label:a.label, detail:a.detail??'', value:`open ${a.value}`});
      for (const q of (apiCompletions.queues ?? [])) if (!arg || fuzzyMatch(arg, q.label)) items.push({label:q.label, detail:`queue (${q.detail??''})`, value:`open ${q.value}`});
      return items.slice(0, 20);
    }

    // Special case: "focus" has custom items
    if (cmd === "focus") {
      return "h j k l next prev".split(" ").filter(d => !arg || d.startsWith(arg)).map(d => ({label:d, detail:`Focus ${d}`, value:`focus ${d}`}));
    }

    // Use completionCtx from command definition
    if (cmdDef.completionCtx) {
      const items = apiCompletions[cmdDef.completionCtx] ?? [];
      return items.filter(it => !arg || fuzzyMatch(arg, it.label))
        .map(it => ({label: it.label, detail: it.detail ?? "", value: `${cmd} ${it.value}`}))
        .slice(0, 20);
    }

    return [];
  }, [mode, cmdInput, apiCompletions]);

  const [cmdError, setCmdError] = useState<string|null>(null);

  const submitCommand = useCallback((value?: string) => {
    const input = value ?? cmdInput;
    const err = execCommand(input);
    if (err) { setCmdError(err); setTimeout(() => setCmdError(null), 3000); }
    setMode("normal"); setCmdInput("");
  }, [cmdInput, execCommand]);

  return (
    <DataCtx.Provider value={ctx}>
      <div className="app-root" data-theme={theme}>
        <div className="pane-root">
          <PaneRenderer node={tree} setTree={setTree} />
        </div>

        {mode !== "normal" && mode !== "command" && (
          <div className="mode-indicator">{mode === "resize" ? "RESIZE hjkl / Esc" : "SWAP hjkl / Esc"}</div>
        )}

        {mode === "command" && (
          <div className="cmd-overlay">
            <div className="cmd-palette">
              <div className="cmd-input-row">
                <span className="cmd-colon">:</span>
                <input ref={cmdRef} className="cmd-input" value={cmdInput} autoFocus spellCheck={false}
                  onChange={e => { setCmdInput(e.target.value); setCmdSelectedIdx(0); }}
                  onKeyDown={e => {
                    if (e.key === "Escape") { setMode("normal"); setCmdInput(""); return; }
                    if (e.key === "Enter") { e.preventDefault(); if (cmdMatches[cmdSelectedIdx]) submitCommand(cmdMatches[cmdSelectedIdx].value); else submitCommand(); return; }
                    if (e.key === "ArrowDown" || (e.ctrlKey && e.key === "n") || (e.ctrlKey && e.key === "j")) { e.preventDefault(); setCmdSelectedIdx(i => Math.min(i+1, cmdMatches.length-1)); return; }
                    if (e.key === "ArrowUp" || (e.ctrlKey && e.key === "p") || (e.ctrlKey && e.key === "k")) { e.preventDefault(); setCmdSelectedIdx(i => Math.max(i-1, 0)); return; }
                    if (e.key === "Tab") { e.preventDefault(); if (cmdMatches[cmdSelectedIdx]) { setCmdInput(cmdMatches[cmdSelectedIdx].value); setCmdSelectedIdx(0); } return; }
                  }}
                />
              </div>
              {cmdMatches.length > 0 && (
                <CmdCompletions items={cmdMatches} selectedIdx={cmdSelectedIdx}
                  onSelect={setCmdSelectedIdx} onSubmit={submitCommand} />
              )}
            </div>
          </div>
        )}

        {cmdError && <div className="cmd-error">{cmdError}</div>}

        {ctxMenu && (
          <div className="ctx-menu" style={{left:ctxMenu.x, top:ctxMenu.y}}>
            <button onClick={() => {splitPane(ctxMenu.paneId,"v");setCtxMenu(null);}}>Split Right <kbd>|</kbd></button>
            <button onClick={() => {splitPane(ctxMenu.paneId,"h");setCtxMenu(null);}}>Split Below <kbd>-</kbd></button>
            <div className="ctx-sep"/>
            <button onClick={() => {setPaneView(ctxMenu.paneId,{kind:"angl-list"});setCtxMenu(null);}}>Angl List</button>
            <button onClick={() => {setPaneView(ctxMenu.paneId,{kind:"schedg-list"});setCtxMenu(null);}}>Queue List</button>
            <div className="ctx-sep"/>
            <button onClick={() => {setMode("resize");setCtxMenu(null);}}>Resize <kbd>^R</kbd></button>
            <button onClick={() => {setMode("swap");setCtxMenu(null);}}>Swap <kbd>^X</kbd></button>
            <button onClick={() => {setMode("command");setCmdInput("");setCtxMenu(null);setTimeout(()=>cmdRef.current?.focus(),0);}}>Command <kbd>:</kbd></button>
            <div className="ctx-sep"/>
            <button className="ctx-danger" onClick={() => {closePane(ctxMenu.paneId);setCtxMenu(null);}}>Close <kbd>^W</kbd></button>
          </div>
        )}
      </div>
    </DataCtx.Provider>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Pane Renderer
// ═══════════════════════════════════════════════════════════════════

function PaneRenderer({ node, setTree }: { node: PaneNode; setTree: React.Dispatch<React.SetStateAction<PaneNode>> }) {
  if (node.type === "leaf") return <PaneLeaf id={node.id} view={node.view} />;
  const isH = node.direction === "h";
  const containerRef = useRef<HTMLDivElement>(null);
  const onMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    const c = containerRef.current; if (!c) return;
    const rect = c.getBoundingClientRect();
    const onMove = (me: MouseEvent) => { setTree(t => updateRatio(t, node.id, isH ? (me.clientY-rect.top)/rect.height : (me.clientX-rect.left)/rect.width)); };
    const onUp = () => { window.removeEventListener("mousemove",onMove); window.removeEventListener("mouseup",onUp); };
    window.addEventListener("mousemove",onMove); window.addEventListener("mouseup",onUp);
  }, [isH, node.id, setTree]);

  const style = isH
    ? { gridTemplateRows: `${node.ratio*100}% 4px ${(1-node.ratio)*100-0.5}%`, gridTemplateColumns: "1fr" }
    : { gridTemplateColumns: `${node.ratio*100}% 4px ${(1-node.ratio)*100-0.5}%`, gridTemplateRows: "1fr" };

  return (
    <div className="pane-split" style={style} ref={containerRef}>
      <div className="pane-child"><PaneRenderer node={node.a} setTree={setTree} /></div>
      <div className={`pane-handle ${isH?"pane-handle-h":"pane-handle-v"}`} onMouseDown={onMouseDown} />
      <div className="pane-child"><PaneRenderer node={node.b} setTree={setTree} /></div>
    </div>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Pane Leaf
// ═══════════════════════════════════════════════════════════════════

function PaneLeaf({ id, view }: { id: string; view: ViewType }) {
  const { focusedPaneId, setFocusedPaneId, setView, splitPane, closePane } = useContext(DataCtx);
  const focused = focusedPaneId === id;
  const label = view.kind === "angl-list" ? "angls" : view.kind === "schedg-list" ? "queues" : view.kind === "angl-detail" ? view.name : view.kind === "schedg-detail" ? view.queue : "conversation";

  return (
    <div className={`pane-leaf ${focused?"pane-focused":""}`} onClick={() => setFocusedPaneId(id)} data-pane-id={id}>
      <div className="pane-toolbar">
        <span className="pane-label">{label}</span>
        <span style={{flex:1}}/>
        <button className="pane-btn" onClick={() => splitPane(id,"v")} title="Split right">|</button>
        <button className="pane-btn" onClick={() => splitPane(id,"h")} title="Split below">&ndash;</button>
        <button className="pane-btn pane-close" onClick={() => closePane(id)} title="Close">&times;</button>
      </div>
      <div className="pane-content">
        {view.kind === "angl-list" && <AnglListView paneId={id} />}
        {view.kind === "schedg-list" && <SchedgListView paneId={id} />}
        {view.kind === "angl-detail" && <AnglDetailView paneId={id} name={view.name} />}
        {view.kind === "schedg-detail" && <SchedgDetailView paneId={id} queue={view.queue} />}
        {view.kind === "conversation" && <ConversationView paneId={id} orchardUrl={view.orchardUrl} envId={view.envId} convId={view.convId} anglName={view.anglName} />}
      </div>
    </div>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Angl List
// ═══════════════════════════════════════════════════════════════════

function AnglListView({ paneId }: { paneId: string }) {
  const { angls, openIn, focusedPaneId } = useContext(DataCtx);
  const [search, setSearch] = useState("");
  const [stateFilter, setStateFilter] = useState<string|null>(null);
  const [focusedIdx, setFocusedIdx] = useState(-1);
  const searchRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const focused = focusedPaneId === paneId;

  const filtered = useMemo(() => {
    let list = angls;
    if (stateFilter) list = list.filter(a => a.state === stateFilter);
    if (!search) return list;
    return list.filter(a => fuzzyMatch(search, a.name) || fuzzyMatch(search, a.charge ?? "") || (a.tags ?? []).some(t => fuzzyMatch(search, t)));
  }, [angls, search, stateFilter]);

  const counts = useMemo(() => { const c: Record<string,number> = {}; for (const a of angls) c[a.state]=(c[a.state]??0)+1; return c; }, [angls]);

  const openAngl = useCallback((name: string, mode: "current"|"split-h"|"split-v") => openIn(paneId, {kind:"angl-detail",name}, mode), [paneId, openIn]);

  useEffect(() => {
    if (!focused) return;
    const handler = (e: KeyboardEvent) => {
      const inInput = (e.target as HTMLElement)?.tagName === "INPUT";
      if (e.key === "/" && !inInput) { e.preventDefault(); searchRef.current?.focus(); return; }
      if (e.key === "Escape" && inInput) { (e.target as HTMLElement).blur(); return; }
      if (inInput) return;
      if (e.key==="j"||e.key==="ArrowDown") { e.preventDefault(); setFocusedIdx(i=>Math.min(i+1,filtered.length-1)); return; }
      if (e.key==="k"||e.key==="ArrowUp") { e.preventDefault(); setFocusedIdx(i=>Math.max(i<0?0:i-1,0)); return; }
      if ((e.key==="Enter"||e.key==="*") && focusedIdx>=0 && filtered[focusedIdx]) { e.preventDefault(); openAngl(filtered[focusedIdx].name,"current"); return; }
      if (e.key==="l" && !e.ctrlKey && focusedIdx>=0 && filtered[focusedIdx]) { e.preventDefault(); openAngl(filtered[focusedIdx].name,"split-v"); return; }
      if (e.key==="s" && focusedIdx>=0 && filtered[focusedIdx]) { e.preventDefault(); openAngl(filtered[focusedIdx].name,"split-h"); return; }
      if (e.key==="c" && focusedIdx>=0 && filtered[focusedIdx]) { copyText(filtered[focusedIdx].name); return; }
      const fk: Record<string,string|null> = {"1":"running","2":"backoff","3":"stopped","4":"disabled","5":"failed","0":null};
      if (fk[e.key]!==undefined) { setStateFilter(fk[e.key]); setFocusedIdx(-1); return; }
    };
    window.addEventListener("keydown", handler); return () => window.removeEventListener("keydown", handler);
  }, [focused, filtered, focusedIdx, openAngl]);

  useEffect(() => setFocusedIdx(-1), [search, stateFilter]);
  useEffect(() => { listRef.current?.querySelector(".focused")?.scrollIntoView({block:"nearest"}); }, [focusedIdx]);

  return (
    <div className="list-panel">
      <div className="counts-bar">
        {(["running","backoff","stopped","disabled","failed"] as const).map((st,i) => (
          <button key={st} className={`count-btn ${stateFilter===st?"active":""}`} onClick={() => {setStateFilter(stateFilter===st?null:st);setFocusedIdx(-1);}}>
            <span className="count-key">{i+1}</span><span className="count-label">{st}</span><span className={`count-num count-${st}`}>{counts[st]??0}</span>
          </button>
        ))}
        <button className={`count-btn ${stateFilter===null?"active":""}`} onClick={() => {setStateFilter(null);setFocusedIdx(-1);}}>
          <span className="count-key">0</span><span className="count-label">all</span><span className="count-num">{angls.length}</span>
        </button>
      </div>
      <SearchBar ref={searchRef} value={search} onChange={setSearch} count={filtered.length} placeholder="Filter angls... ( / )" />
      <div className="item-list" ref={listRef}>
        {filtered.length === 0 && <p className="empty">No angls{stateFilter?` in "${stateFilter}"`:""}</p>}
        {filtered.map((a,i) => (
          <div key={a.name} className={`item-row ${i===focusedIdx?"focused":""}`}
            onClick={() => openAngl(a.name,"current")}
            onAuxClick={e => { if (e.button===1) { e.preventDefault(); openAngl(a.name,"split-v"); }}}>
            <span className="item-state" style={{color:stateColor(a.state)}}>{a.state}</span>
            <span className="item-name">{a.name}</span>
            {a.interval && <span className="item-badge">{a.interval}</span>}
            {a.state==="backoff" && a.next_run_in && <span className="item-badge item-badge-accent">in {a.next_run_in}</span>}
            {a.uptime && <span className="item-dim" style={{color:"var(--green-bright)"}}>{a.uptime}</span>}
            <span className="item-desc">{a.charge??""}</span>
            {(a.tags??[]).map(t => <span key={t} className="item-tag">{t}</span>)}
            <CopyBtn text={a.name} />
          </div>
        ))}
      </div>
    </div>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Schedg List
// ═══════════════════════════════════════════════════════════════════

function SchedgListView({ paneId }: { paneId: string }) {
  const { queues, openIn, focusedPaneId } = useContext(DataCtx);
  const [focusedIdx, setFocusedIdx] = useState(-1);
  const [search, setSearch] = useState("");
  const searchRef = useRef<HTMLInputElement>(null);
  const focused = focusedPaneId === paneId;

  const filtered = useMemo(() => !search ? queues : queues.filter(q => fuzzyMatch(search, q.name)), [queues, search]);

  const openQueue = useCallback((name: string, mode: "current"|"split-h"|"split-v") => openIn(paneId, {kind:"schedg-detail",queue:name}, mode), [paneId, openIn]);

  useEffect(() => {
    if (!focused) return;
    const handler = (e: KeyboardEvent) => {
      if ((e.target as HTMLElement)?.tagName === "INPUT") {
        if (e.key === "Escape") { (e.target as HTMLElement).blur(); } return;
      }
      if (e.key === "/") { e.preventDefault(); searchRef.current?.focus(); return; }
      if (e.key==="j"||e.key==="ArrowDown") { e.preventDefault(); setFocusedIdx(i=>Math.min(i+1,filtered.length-1)); return; }
      if (e.key==="k"||e.key==="ArrowUp") { e.preventDefault(); setFocusedIdx(i=>Math.max(i<0?0:i-1,0)); return; }
      if ((e.key==="Enter"||e.key==="*") && focusedIdx>=0 && filtered[focusedIdx]) { e.preventDefault(); openQueue(filtered[focusedIdx].name,"current"); return; }
      if (e.key==="l" && !e.ctrlKey && focusedIdx>=0 && filtered[focusedIdx]) { e.preventDefault(); openQueue(filtered[focusedIdx].name,"split-v"); return; }
    };
    window.addEventListener("keydown", handler); return () => window.removeEventListener("keydown", handler);
  }, [focused, filtered, focusedIdx, openQueue]);

  useEffect(() => setFocusedIdx(-1), [search]);

  return (
    <div className="list-panel">
      <SearchBar ref={searchRef} value={search} onChange={setSearch} count={filtered.length} placeholder="Filter queues... ( / )" />
      <div className="item-list">
        {filtered.length === 0 && <p className="empty">No queues.</p>}
        {filtered.map((q,i) => (
          <div key={q.name} className={`item-row queue-card ${i===focusedIdx?"focused":""}`}
            onClick={() => openQueue(q.name,"current")}
            onAuxClick={e => { if (e.button===1) { e.preventDefault(); openQueue(q.name,"split-v"); }}}>
            <span className="item-name">{q.name}</span>
            <span className="item-badge">{q.driver}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Angl Detail
// ═══════════════════════════════════════════════════════════════════

function AnglDetailView({ paneId, name }: { paneId: string; name: string }) {
  const { angls, setView, openIn } = useContext(DataCtx);
  const angl = useMemo(() => angls.find(a => a.name === name) ?? null, [angls, name]);
  const [tailLines, setTailLines] = useState<string[]>([]);
  const [schedgSnapshot, setSchedgSnapshot] = useState<QueueSnapshot|null>(null);
  const [msgText, setMsgText] = useState("");
  const [sending, setSending] = useState(false);
  const [infoOpen, setInfoOpen] = useState(false);

  useEffect(() => {
    setTailLines([]);
    const es = new EventSource(`/api/angls/${encodeURIComponent(name)}/tail?history=200`);
    es.onmessage = e => setTailLines(p => [...p.slice(-999), e.data]);
    es.onerror = () => { es.close(); };
    return () => es.close();
  }, [name]);

  const schedgName = `angl.${name}`;
  useSSE<QueueSnapshot>(`/api/queues/${encodeURIComponent(schedgName)}/events`, setSchedgSnapshot);

  const openQueue = useCallback(() => openIn(paneId, {kind:"schedg-detail", queue: schedgName}, "split-v"), [paneId, schedgName, openIn]);

  // Detect conversation mode from tags (must be before sendMessage which references these)
  const convTag = (angl?.tags ?? []).find(t => t.startsWith("conversation:"));
  const convId = convTag?.slice("conversation:".length);
  const envId = "56c322d4-942e-e277-ab14-99f4a5a4f3ab"; // TODO: from config

  const sendMessage = useCallback((mode: "interrupt" | "wake" = "interrupt") => {
    if (!msgText.trim() || sending) return;
    setSending(true);
    fetch("/api/rpc", { method: "POST", headers: {"Content-Type":"application/json"},
      body: JSON.stringify({jsonrpc:"2.0",method:"message",params:{name,prompt:msgText.trim(),from:"web",mode},id:1})
    }).then(() => { setMsgText(""); setSending(false); }).catch(() => setSending(false));
  }, [name, msgText, sending]);

  const inflightTask = schedgSnapshot?.inflight?.[0] ?? null;
  const totalMessages = schedgSnapshot ? (schedgSnapshot.counts.ready??0)+(schedgSnapshot.counts.inflight??0)+(schedgSnapshot.counts.completed??0) : 0;

  return (
    <div className="chat-layout">
      {/* Topbar */}
      <div className="chat-topbar">
        <button className="back-btn" onClick={() => setView(paneId,{kind:"angl-list"})}>&larr;</button>
        <span className="topbar-name">{name}</span>
        {angl && <span className="badge" style={{borderColor:stateColor(angl.state),color:stateColor(angl.state)}}>{angl.state}</span>}
        {angl?.interval && <span className="item-badge">{angl.interval}</span>}
        {angl?.state==="backoff" && angl.next_run_in && <span className="item-badge item-badge-accent">in {angl.next_run_in}</span>}
        {(angl?.tags??[]).filter(t => !t.startsWith("conversation:")).map(t => {
          const isSchedg = t.startsWith("schedg:");
          return isSchedg
            ? <button key={t} className="tag-link" onClick={() => openIn(paneId,{kind:"schedg-detail",queue:t.slice(7)},"split-v")}>{t}</button>
            : <span key={t} className="item-tag">{t}</span>;
        })}
        <span style={{flex:1}}/>
        <button className={`info-toggle ${infoOpen?"info-toggle-active":""}`} onClick={() => setInfoOpen(o=>!o)} title="Toggle info panel">i</button>
      </div>

      {angl?.charge && <div className="chat-charge">{angl.charge}</div>}

      {/* Info flyout */}
      {infoOpen && angl && (
        <div className="info-flyout">
          <div className="meta-grid">
            {angl.pid ? <MetaRow k="PID" v={String(angl.pid)} /> : null}
            {angl.uptime && <MetaRow k="Uptime" v={angl.uptime} />}
            {angl.started && <MetaRow k="Started" v={relativeTime(angl.started)} />}
            {angl.last_exit && <MetaRow k="Last Exit" v={relativeTime(angl.last_exit)} />}
            {angl.state==="backoff" && angl.next_run_in && <MetaRow k="Next Run" v={`in ${angl.next_run_in}`} accent />}
            <MetaRow k="Restarts" v={angl.max_restarts?`${angl.restarts}/${angl.max_restarts}`:String(angl.restarts)} />
            {angl.lifetime && <MetaRow k="Lifetime" v={angl.lifetime} />}
            {angl.created_at && <MetaRow k="Created" v={relativeTime(angl.created_at)} />}
          </div>
          {inflightTask && (
            <div className="inflight-msg">
              <div className="inflight-msg-header">
                <span className="badge badge-inflight">in-flight</span>
                <span className="inflight-msg-title">{inflightTask.title || `#${inflightTask.id}`}</span>
              </div>
              {inflightTask.description && (
                <div className="inflight-msg-body detail-md">
                  <ReactMarkdown remarkPlugins={[remarkGfm]}>{inflightTask.description}</ReactMarkdown>
                </div>
              )}
            </div>
          )}
        </div>
      )}

      {/* Main content: conversation chat or tail log */}
      {convId ? (
        <ConversationStream envId={envId} convId={convId} />
      ) : (
        <TailView lines={tailLines} />
      )}

      {/* Bottom bar: input + pills */}
      <div className="chat-bottom">
        <input className="chat-input" placeholder={`Message ${name}...`} value={msgText}
          onChange={e => setMsgText(e.target.value)}
          onKeyDown={e => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault(); e.stopPropagation();
              sendMessage(e.altKey ? "wake" : "interrupt");
            }
          }}
          spellCheck={false} />
        <button className="chat-pill chat-pill-send" onClick={() => sendMessage("interrupt")} disabled={sending || !msgText.trim()} title="Send + interrupt (Enter)">
          {sending ? "\u2026" : "\u25B6"}
        </button>
        <button className="chat-pill chat-pill-queue" onClick={() => sendMessage("wake")} disabled={sending || !msgText.trim()} title="Queue + wake (Alt+Enter)">
          queue
        </button>
        <button className="chat-pill chat-pill-view" onClick={openQueue} title="View message queue">
          {totalMessages > 0 ? `\u2709 ${totalMessages}` : "\u2709"}
        </button>
      </div>
    </div>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Schedg Detail
// ═══════════════════════════════════════════════════════════════════

const TABS: ListTab[] = ["ready","blocked","inflight","dead","completed"];
const TAB_LABELS: Record<ListTab,string> = {ready:"Ready",blocked:"Blocked",inflight:"In-Flight",dead:"Dead",completed:"Completed"};

function schedgRpc(method: string, params: Record<string,any>): Promise<any> {
  return fetch("/api/rpc", { method: "POST", headers: {"Content-Type":"application/json"},
    body: JSON.stringify({jsonrpc:"2.0",method,params,id:1})
  }).then(r => r.json()).then(d => { if (d.error) throw new Error(d.error.message); return d.result; });
}

function SchedgDetailView({ paneId, queue }: { paneId: string; queue: string }) {
  const { setView, focusedPaneId } = useContext(DataCtx);
  const [snapshot, setSnapshot] = useState<QueueSnapshot|null>(null);
  const [activeTab, setActiveTab] = useState<ListTab>("ready");
  const [search, setSearch] = useState("");
  const [focusedIdx, setFocusedIdx] = useState(-1);
  const [expanded, setExpanded] = useState<string|null>(null);
  const [taskMenu, setTaskMenu] = useState<{x:number;y:number;task:TaskView;tab:ListTab}|null>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const focused = focusedPaneId === paneId;

  useSSE<QueueSnapshot>(`/api/queues/${encodeURIComponent(queue)}/events`, setSnapshot);

  const currentTasks = useMemo(() => {
    if (!snapshot) return [];
    const lists: Record<ListTab,TaskView[]> = {ready:snapshot.ready,blocked:snapshot.blocked,inflight:snapshot.inflight,dead:snapshot.dead,completed:snapshot.completed};
    const all = [...(lists[activeTab]||[])].sort((a,b) => a.id.localeCompare(b.id, undefined, {numeric:true}));
    if (!search) return all;
    return all.filter(t => fuzzyMatch(search,t.title) || fuzzyMatch(search,t.id) || fuzzyMatch(search,t.description??""));
  }, [snapshot, activeTab, search]);

  useEffect(() => {
    if (!focused) return;
    const handler = (e: KeyboardEvent) => {
      if ((e.target as HTMLElement)?.tagName === "INPUT") { if (e.key==="Escape")(e.target as HTMLElement).blur(); return; }
      if (e.key==="/") { e.preventDefault(); searchRef.current?.focus(); return; }
      if (e.key==="j"||e.key==="ArrowDown") { e.preventDefault(); setFocusedIdx(i=>Math.min(i+1,currentTasks.length-1)); return; }
      if (e.key==="k"||e.key==="ArrowUp") { e.preventDefault(); setFocusedIdx(i=>Math.max(i<0?0:i-1,0)); return; }
      if (e.key==="Enter" && focusedIdx>=0 && currentTasks[focusedIdx]) { e.preventDefault(); setExpanded(x=>x===currentTasks[focusedIdx].id?null:currentTasks[focusedIdx].id); return; }
      if (e.key==="x" && !e.ctrlKey && focusedIdx>=0 && currentTasks[focusedIdx]) { e.preventDefault(); setExpanded(x=>x===currentTasks[focusedIdx].id?null:currentTasks[focusedIdx].id); return; }
      const tk: Record<string,ListTab> = {"1":"ready","2":"blocked","3":"inflight","4":"dead","5":"completed"};
      if (tk[e.key]) { setActiveTab(tk[e.key]); setFocusedIdx(-1); return; }
    };
    window.addEventListener("keydown", handler); return () => window.removeEventListener("keydown", handler);
  }, [focused, currentTasks, focusedIdx]);

  useEffect(() => setFocusedIdx(-1), [activeTab, search]);
  useEffect(() => { listRef.current?.querySelector(".focused")?.scrollIntoView({block:"nearest"}); }, [focusedIdx]);

  return (
    <div className="detail-pane">
      <div className="detail-topbar">
        <button className="back-btn" onClick={() => setView(paneId,{kind:"schedg-list"})}>&larr;</button>
        <span className="topbar-name">{queue}</span>
        {snapshot && <span className="item-dim">{snapshot.counts.ready+snapshot.counts.inflight+snapshot.counts.blocked} active</span>}
      </div>
      {snapshot && (
        <>
          <div className="counts-bar">
            {TABS.map((tab,i) => (
              <button key={tab} className={`count-btn ${activeTab===tab?"active":""}`} onClick={() => {setActiveTab(tab);setFocusedIdx(-1);}}>
                <span className="count-key">{i+1}</span><span className="count-label">{TAB_LABELS[tab]}</span>
                <span className={`count-num count-${tab}`}>{snapshot.counts[tab]??0}</span>
              </button>
            ))}
          </div>
          <SearchBar ref={searchRef} value={search} onChange={setSearch} count={currentTasks.length} placeholder="Filter tasks... ( / )" />
          <div className="item-list" ref={listRef}>
            {currentTasks.length===0 && <p className="empty">No tasks in {TAB_LABELS[activeTab].toLowerCase()}</p>}
            {currentTasks.map((t,i) => (
              <div key={t.id} className={`task-expandable ${i===focusedIdx?"focused":""}`}>
                <div className={`item-row state-${activeTab}`}
                  onClick={() => setExpanded(x=>x===t.id?null:t.id)}
                  onContextMenu={e => { e.preventDefault(); e.stopPropagation(); setTaskMenu({x:e.clientX,y:e.clientY,task:t,tab:activeTab}); }}>
                  <span className="item-id">#{t.id}</span>
                  <span className={`prio p${Math.min(t.priority,9)}`}>p{t.priority}</span>
                  <span className="item-name">{t.title||`Task #${t.id}`}</span>
                  {t.caller && <span className="item-badge">{t.caller}</span>}
                  {t.leasedAt && <span className="item-dim" style={{color:"var(--accent-bright)"}}>{relativeTime(t.leasedAt)}</span>}
                  <span className="mini-task-expand">{expanded===t.id?"\u25B4":"\u25BE"}</span>
                  <CopyBtn text={t.title||t.id} />
                </div>
                {expanded===t.id && (
                  <div className="task-expanded-body">
                    {t.description && <div className="detail-body"><div className="detail-md"><ReactMarkdown remarkPlugins={[remarkGfm]}>{t.description}</ReactMarkdown></div></div>}
                    <div className="meta-grid">
                      {t.submitted && <MetaRow k="Submitted" v={relativeTime(t.submitted)} />}
                      {t.leasedAt && <MetaRow k="Leased" v={relativeTime(t.leasedAt)} />}
                      {t.caller && <MetaRow k="Caller" v={t.caller} />}
                      {(t.attempts??0)>0 && <MetaRow k="Attempts" v={String(t.attempts)} />}
                      {t.reason && <MetaRow k="Reason" v={t.reason} accent />}
                    </div>
                    <div className="task-actions">
                      {activeTab === "inflight" && <>
                        <button className="task-action" onClick={() => schedgRpc("schedg-complete",{queue,id:t.id})}>Complete</button>
                        <button className="task-action" onClick={() => schedgRpc("schedg-cancel",{queue,id:t.id})}>Return to queue</button>
                        <button className="task-action task-action-danger" onClick={() => schedgRpc("schedg-fail",{queue,id:t.id})}>Fail</button>
                      </>}
                      {activeTab === "ready" && <button className="task-action task-action-danger" onClick={() => schedgRpc("schedg-fail",{queue,id:t.id})}>Bury</button>}
                      {activeTab === "dead" && <button className="task-action" onClick={() => schedgRpc("schedg-requeue",{queue,id:t.id})}>Requeue</button>}
                    </div>
                  </div>
                )}
              </div>
            ))}
          </div>
        </>
      )}

      {taskMenu && <>
        <div className="ctx-backdrop" onClick={() => setTaskMenu(null)} />
        <div className="ctx-menu" style={{left:taskMenu.x,top:taskMenu.y}}>
          <div className="ctx-menu-header">#{taskMenu.task.id}</div>
          {taskMenu.tab === "inflight" && <>
            <button onClick={() => { schedgRpc("schedg-complete",{queue,id:taskMenu.task.id}); setTaskMenu(null); }}>Complete</button>
            <button onClick={() => { schedgRpc("schedg-cancel",{queue,id:taskMenu.task.id}); setTaskMenu(null); }}>Return to queue</button>
            <button className="ctx-danger" onClick={() => { schedgRpc("schedg-fail",{queue,id:taskMenu.task.id}); setTaskMenu(null); }}>Fail</button>
          </>}
          {taskMenu.tab === "ready" && <>
            <button className="ctx-danger" onClick={() => { schedgRpc("schedg-fail",{queue,id:taskMenu.task.id}); setTaskMenu(null); }}>Bury</button>
          </>}
          {taskMenu.tab === "dead" && <>
            <button onClick={() => { schedgRpc("schedg-requeue",{queue,id:taskMenu.task.id}); setTaskMenu(null); }}>Requeue</button>
          </>}
          <div className="ctx-sep"/>
          <button onClick={() => { copyText(`#${taskMenu.task.id} ${taskMenu.task.title}`); setTaskMenu(null); }}>Copy title</button>
          {taskMenu.task.description && <button onClick={() => { copyText(taskMenu.task.description!); setTaskMenu(null); }}>Copy description</button>}
        </div>
      </>}
    </div>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Conversation Components (defined before AnglDetailView to avoid hoisting issues)
// ═══════════════════════════════════════════════════════════════════

// Parse content array into ordered blocks for sequential rendering
type ContentBlock = 
  | { kind: "text"; value: string }
  | { kind: "thinking"; value: string }
  | { kind: "toolUse"; id: string };

function parseContentBlocks(content: any[]): ContentBlock[] {
  const blocks: ContentBlock[] = [];
  for (const item of content) {
    if (typeof item === "string") {
      // Merge adjacent text blocks
      const last = blocks[blocks.length - 1];
      if (last?.kind === "text") { last.value += item; }
      else { blocks.push({ kind: "text", value: item }); }
      continue;
    }
    if (item && typeof item === "object") {
      if (item.type === "thinking") { blocks.push({ kind: "thinking", value: item.value || "" }); continue; }
      if (item.type === "toolCall") { blocks.push({ kind: "toolUse", id: item.id }); continue; }
      // Unknown object - render as text
      const val = String(item.value || item.text || "");
      if (val) {
        const last = blocks[blocks.length - 1];
        if (last?.kind === "text") { last.value += val; }
        else { blocks.push({ kind: "text", value: val }); }
      }
    }
  }
  return blocks;
}

function ConvTurnViewEarly({ turn }: { turn: ConvTurn }) {
  const isStreaming = turn.status === "Streaming";
  const blocks = useMemo(() => parseContentBlocks(turn.content || []), [turn.content]);
  const toolCalls = turn.toolCalls || {};

  return (
    <div className={`conv-turn ${isStreaming ? "conv-streaming" : ""}`}>
      <div className="conv-user"><span className="conv-user-label">you</span><span className="conv-user-text">{turn.user}</span></div>
      <div className="conv-blocks">
        {blocks.map((block, i) => {
          switch (block.kind) {
            case "text":
              return <div key={i} className="conv-text-block"><div className="conv-assistant-md detail-md"><ReactMarkdown remarkPlugins={[remarkGfm]}>{block.value}</ReactMarkdown></div></div>;
            case "thinking":
              return <ConvThinkingBlock key={i} text={block.value} />;
            case "toolUse":
              return <ConvToolBlock key={i} id={block.id} tc={(toolCalls as any)[block.id]} />;
          }
        })}
        {Object.keys(toolCalls).filter(id => !blocks.some(b => b.kind === "toolUse" && b.id === id)).map(id => (
          <ConvToolBlock key={id} id={id} tc={(toolCalls as any)[id]} />
        ))}
      </div>
      <div className="conv-status">
        <span className={`conv-status-badge conv-status-${turn.status.toLowerCase()}`}>{turn.status.toLowerCase()}</span>
        {turn.timestamp && <span className="conv-timestamp">{relativeTime(turn.timestamp)}</span>}
      </div>
    </div>
  );
}

function ConvThinkingBlock({ text }: { text: string }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="conv-thinking-block">
      <button className="conv-thinking-toggle" onClick={() => setOpen(!open)}>{open ? "\u25B4" : "\u25BE"} thinking</button>
      {open && <div className="conv-thinking-body">{text}</div>}
    </div>
  );
}

function ConvToolBlock({ id, tc }: { id: string; tc: any }) {
  const name = tc?.name || tc?.Name || id;
  const args = tc?.args || tc?.Args || "";
  const result = tc?.result || tc?.Result || "";
  const error = tc?.error || tc?.Error || "";
  const statusRaw = tc?.status || tc?.Status;
  const isDone = statusRaw === "Completed" || statusRaw === 1 || statusRaw === "Failed" || statusRaw === 2 || !!result || !!error;
  const isFailed = statusRaw === "Failed" || statusRaw === 2 || !!error;
  const statusLabel = isFailed ? "failed" : isDone ? "done" : "running";

  // Open while running, auto-fold on completion
  const [open, setOpen] = useState(!isDone);
  const prevDone = useRef(isDone);
  useEffect(() => {
    if (isDone && !prevDone.current) setOpen(false);
    prevDone.current = isDone;
  }, [isDone]);

  return (
    <div className={`conv-tool-block ${isFailed ? "conv-tool-error" : ""} ${!isDone ? "conv-tool-active" : ""}`}>
      <div className="conv-tool-header" onClick={() => setOpen(!open)}>
        <span className={`conv-tool-status-dot ${isFailed ? "dot-fail" : isDone ? "dot-ok" : "dot-run"}`} />
        <span className="conv-tool-name">{name}</span>
        <span className={`conv-tool-status ${isFailed ? "conv-tool-fail" : isDone ? "conv-tool-ok" : "conv-tool-run"}`}>{statusLabel}</span>
        <span className="conv-tool-chevron">{open ? "\u25B4" : "\u25BE"}</span>
      </div>
      {open && (
        <div className="conv-tool-body">
          {args && <div className="conv-tool-section"><span className="conv-tool-section-label">args</span><pre className="conv-tool-args">{typeof args === "string" ? args : JSON.stringify(args, null, 2)}</pre></div>}
          {result && <div className="conv-tool-section"><span className="conv-tool-section-label">result</span><pre className="conv-tool-result">{typeof result === "string" ? result : JSON.stringify(result, null, 2)}</pre></div>}
          {error && <div className="conv-tool-section"><span className="conv-tool-section-label">error</span><pre className="conv-tool-error-text">{String(error)}</pre></div>}
        </div>
      )}
    </div>
  );
}

function ConversationStream({ envId, convId }: { envId: string; convId: string }) {
  const [turns, setTurns] = useState<ConvTurn[]>([]);
  const [connected, setConnected] = useState(false);
  const [reconnectKey, setReconnectKey] = useState(0);
  const containerRef = useRef<HTMLDivElement>(null);
  const endRef = useRef<HTMLDivElement>(null);
  const [autoScroll, setAutoScroll] = useState(true);

  // Expose reconnect trigger for parent to call after sending
  (ConversationStream as any)._reconnect = () => setReconnectKey(k => k + 1);

  useEffect(() => {
    let cancelled = false;

    // First try a one-shot read to get existing messages
    fetch(`/api/orchard/api/e/${envId}/conversation/${convId}?version=0&index=0`)
      .then(r => r.ok ? r.json() : null)
      .then(data => {
        if (data?.messages) setTurns(data.messages);
      })
      .catch(() => {});

    // Then connect to SSE stream for live updates
    const url = `/api/orchard/api/e/${envId}/conversation/${convId}/stream?version=0&index=0`;
    async function connect() {
      try {
        const resp = await fetch(url, { headers: { "Accept": "text/event-stream" } });
        if (!resp.ok || !resp.body) { console.warn("conv stream:", resp.status); return; }
        setConnected(true);
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        while (!cancelled) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let idx;
          while ((idx = buffer.indexOf("\n\n")) >= 0) {
            const frame = buffer.slice(0, idx);
            buffer = buffer.slice(idx + 2);
            for (const line of frame.split("\n")) {
              if (line.startsWith("data: ")) {
                try {
                  const read: ConvRead = JSON.parse(line.slice(6));
                  setTurns(prev => [...prev.slice(0, read.startIndex), ...read.messages]);
                } catch {}
              }
            }
          }
        }
      } catch (e) { console.warn("conv error:", e); }
    }
    connect();
    return () => { cancelled = true; };
  }, [envId, convId, reconnectKey]);

  useEffect(() => { if (autoScroll) endRef.current?.scrollIntoView({ behavior: "smooth" }); }, [turns, autoScroll]);
  const handleScroll = useCallback(() => {
    const el = containerRef.current; if (!el) return;
    setAutoScroll(el.scrollHeight - el.scrollTop - el.clientHeight < 60);
  }, []);

  return (
    <div className="conv-stream" ref={containerRef} onScroll={handleScroll}>
      {!connected && turns.length === 0 && <p className="empty">Send a message to start the conversation.</p>}
      {turns.map((turn, i) => (
        <ConvTurnViewEarly key={i} turn={turn} />
      ))}
      <div ref={endRef} />
    </div>
  );
}

// ═══════════════════════════════════════════════════════════════════
// Conversation View (standalone pane)
// ═══════════════════════════════════════════════════════════════════

function ConversationView({ paneId, orchardUrl, envId, convId, anglName }: { paneId: string; orchardUrl: string; envId: string; convId: string; anglName?: string }) {
  const { setView } = useContext(DataCtx);
  const [turns, setTurns] = useState<ConvTurn[]>([]);
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const [connected, setConnected] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const endRef = useRef<HTMLDivElement>(null);
  const [autoScroll, setAutoScroll] = useState(true);

  // SSE stream via the daemon's orchard proxy (handles auth + TLS)
  useEffect(() => {
    let cancelled = false;
    const proxyUrl = `/api/orchard/api/e/${envId}/conversation/${convId}/stream?version=0&index=0`;

    async function connect() {
      try {
        const resp = await fetch(proxyUrl, { headers: { "Accept": "text/event-stream" } });
        if (!resp.ok || !resp.body) {
          console.warn("conversation stream failed:", resp.status);
          return;
        }
        setConnected(true);
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (!cancelled) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });

          let idx;
          while ((idx = buffer.indexOf("\n\n")) >= 0) {
            const frame = buffer.slice(0, idx);
            buffer = buffer.slice(idx + 2);
            for (const line of frame.split("\n")) {
              if (line.startsWith("data: ")) {
                try {
                  const read: ConvRead = JSON.parse(line.slice(6));
                  setTurns(read.messages);
                } catch {}
              }
            }
          }
        }
      } catch (e) {
        console.warn("conversation stream error:", e);
      }
    }

    connect();
    return () => { cancelled = true; };
  }, [envId, convId]);

  // Auto-scroll
  useEffect(() => {
    if (autoScroll) endRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [turns, autoScroll]);

  const handleScroll = useCallback(() => {
    const el = containerRef.current;
    if (!el) return;
    setAutoScroll(el.scrollHeight - el.scrollTop - el.clientHeight < 60);
  }, []);

  // Send message. If this conversation is owned by an angl, route through
  // the schedg queue so the bridge serializes delivery and avoids corrupting
  // the conversation grain with concurrent POSTs.
  const send = useCallback(async () => {
    if (!input.trim() || sending) return;
    setSending(true);
    try {
      if (anglName) {
        await fetch("/api/rpc", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ jsonrpc: "2.0", method: "message", params: { name: anglName, prompt: input.trim(), from: "web" }, id: 1 }),
        });
      } else {
        await fetch(`/api/orchard/api/e/${envId}/conversation/${convId}`, {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ message: input.trim(), modelTier: "large" }),
        });
      }
      setInput("");
    } catch {}
    setSending(false);
  }, [envId, convId, input, sending, anglName]);

  return (
    <div className="conv-pane">
      <div className="conv-header">
        <button className="back-btn" onClick={() => setView(paneId, { kind: "angl-list" })}>&larr;</button>
        <span className="topbar-name">conversation</span>
        <span className="item-dim">{convId.slice(0, 8)}...</span>
        {connected && <span className="conv-live">LIVE</span>}
      </div>

      <div className="conv-messages" ref={containerRef} onScroll={handleScroll}>
        {turns.length === 0 && <p className="empty">No messages yet.</p>}
        {turns.map((turn, i) => (
          <ConvTurnView key={i} turn={turn} />
        ))}
        <div ref={endRef} />
      </div>

      <div className="conv-input-row">
        <input className="conv-input" value={input} placeholder="Send a message..."
          onChange={e => setInput(e.target.value)}
          onKeyDown={e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); e.stopPropagation(); send(); }}}
          spellCheck={false} />
        <button className="conv-send" onClick={send} disabled={sending || !input.trim()}>
          {sending ? "\u2026" : "send"}
        </button>
      </div>
    </div>
  );
}

function ConvTurnView({ turn }: { turn: ConvTurn }) {
  const [thinkingOpen, setThinkingOpen] = useState(false);
  const [toolsOpen, setToolsOpen] = useState(false);
  const toolEntries = Object.entries(turn.toolCalls || {});
  const hasThinking = turn.thinkingContent && turn.thinkingContent.length > 0;
  const isStreaming = turn.status === "Streaming";

  return (
    <div className={`conv-turn ${isStreaming ? "conv-streaming" : ""}`}>
      {/* User message */}
      <div className="conv-user">
        <span className="conv-user-label">you</span>
        <span className="conv-user-text">{turn.user}</span>
      </div>

      {/* Thinking */}
      {hasThinking && (
        <div className="conv-thinking">
          <button className="conv-thinking-toggle" onClick={() => setThinkingOpen(!thinkingOpen)}>
            {thinkingOpen ? "\u25B4" : "\u25BE"} thinking
          </button>
          {thinkingOpen && (
            <div className="conv-thinking-body">
              {turn.thinkingContent!.map((t, i) => <p key={i}>{t}</p>)}
            </div>
          )}
        </div>
      )}

      {/* Tool calls */}
      {toolEntries.length > 0 && (
        <div className="conv-tools">
          <button className="conv-tools-toggle" onClick={() => setToolsOpen(!toolsOpen)}>
            {toolsOpen ? "\u25B4" : "\u25BE"} {toolEntries.length} tool call{toolEntries.length > 1 ? "s" : ""}
          </button>
          {toolsOpen && toolEntries.map(([id, tc]) => (
            <div key={id} className={`conv-tool ${tc.error ? "conv-tool-error" : ""}`}>
              <div className="conv-tool-header">
                <span className="conv-tool-name">{tc.name || id}</span>
                <span className={`conv-tool-status ${tc.status === "Completed" ? "conv-tool-ok" : tc.status === "Failed" ? "conv-tool-fail" : "conv-tool-run"}`}>
                  {tc.status || "running"}
                </span>
              </div>
              {tc.args && <pre className="conv-tool-args">{tc.args.length > 500 ? tc.args.slice(0, 500) + "..." : tc.args}</pre>}
              {tc.result && <pre className="conv-tool-result">{tc.result.length > 500 ? tc.result.slice(0, 500) + "..." : tc.result}</pre>}
              {tc.error && <pre className="conv-tool-error-text">{tc.error}</pre>}
            </div>
          ))}
        </div>
      )}

      {/* Assistant content */}
      {turn.content.length > 0 && (
        <div className="conv-assistant">
          <div className="conv-assistant-md detail-md">
            <ReactMarkdown remarkPlugins={[remarkGfm]}>{turn.content.join("")}</ReactMarkdown>
          </div>
        </div>
      )}

      {/* Status */}
      <div className="conv-status">
        <span className={`conv-status-badge conv-status-${turn.status.toLowerCase()}`}>{turn.status.toLowerCase()}</span>
        {turn.timestamp && <span className="conv-timestamp">{relativeTime(turn.timestamp)}</span>}
      </div>
    </div>
  );
}

function DispatchBtn({ queue, taskId, inline }: { queue: string; taskId: string; inline?: boolean }) {
  const { dispatchTask } = useContext(DataCtx);
  const [fired, setFired] = useState(false);
  return (
    <button className={`dispatch-btn ${inline ? "dispatch-inline" : ""}`}
      onClick={e => { e.stopPropagation(); if (!fired) { setFired(true); dispatchTask(queue, taskId); }}}
      disabled={fired} title="Dispatch an author angl for this task">
      {fired ? "\u2713" : "\u25B6"}
    </button>
  );
}

// Strip ANSI escape sequences for clean display
function stripAnsi(s: string): string {
  return s.replace(/\x1b\[[0-9;]*[A-Za-z]/g, "").replace(/\r/g, "");
}

function CmdCompletions({ items, selectedIdx, onSelect, onSubmit }: {
  items: {label:string;detail:string;value:string}[];
  selectedIdx: number;
  onSelect: (i: number) => void;
  onSubmit: (v: string) => void;
}) {
  const listRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = listRef.current?.children[selectedIdx] as HTMLElement | undefined;
    el?.scrollIntoView({ block: "nearest" });
  }, [selectedIdx]);

  return (
    <div className="cmd-completions" ref={listRef}>
      {items.map((m, i) => (
        <div key={i} className={`cmd-item ${i === selectedIdx ? "cmd-item-selected" : ""}`}
          onMouseEnter={() => onSelect(i)}
          onClick={() => onSubmit(m.value)}>
          <span className="cmd-item-label">{m.label}</span>
          <span className="cmd-item-detail">{m.detail}</span>
        </div>
      ))}
    </div>
  );
}

function TailView({lines}:{lines:string[]}) {
  const endRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [autoScroll, setAutoScroll] = useState(true);
  useEffect(() => { if (autoScroll) endRef.current?.scrollIntoView({behavior:"smooth"}); }, [lines.length, autoScroll]);
  const handleScroll = useCallback(() => { const el=containerRef.current; if(!el) return; setAutoScroll(el.scrollHeight-el.scrollTop-el.clientHeight<40); }, []);
  return (
    <div className="tail" ref={containerRef} onScroll={handleScroll}>
      {lines.length===0 && <p className="empty">No log output yet.</p>}
      {lines.map((line,i) => {
        const clean = stripAnsi(line);
        const cls = clean.includes("ERROR")||clean.includes("error:")?"tail-error":clean.includes("warning")?"tail-warn":"";
        return <div key={i} className={`tail-line ${cls}`}>{clean}</div>;
      })}
      <div ref={endRef} />
    </div>
  );
}
