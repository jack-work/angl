import { useState, useEffect, useCallback, useRef, useMemo, createContext, useContext } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

// ── Types ────────────────────────────────────────────────────────

interface AnglStatus {
  name: string;
  state: string;
  enabled: boolean;
  pid?: number;
  started?: string;
  uptime?: string;
  last_exit?: string;
  next_run?: string;
  next_run_in?: string;
  restarts: number;
  max_restarts?: number;
  interval?: string;
  created_at?: string;
  lifetime?: string;
  charge?: string;
  tags?: string[];
  endpoint?: { http?: string };
}

interface TaskView {
  id: string;
  title: string;
  description?: string;
  priority: number;
  submitted?: string;
  attempts?: number;
  cancels?: number;
  reason?: string;
  leasedAt?: string;
  caller?: string;
  kv?: Record<string, string>;
}

interface DepEdge { from: string; to: string; }

interface QueueSnapshot {
  name: string;
  driver: string;
  ready: TaskView[];
  blocked: TaskView[];
  inflight: TaskView[];
  dead: TaskView[];
  completed: TaskView[];
  deps: DepEdge[];
  blockedBy: Record<string, string[]>;
  counts: Record<string, number>;
  dbMeta?: Record<string, string>;
  snapshotAt: string;
}

interface QueueEntry { name: string; driver: string; path: string; comparator: string; }

type Theme = "crimson" | "azure";
type Section = "angl" | "schedg";
type ListTab = "ready" | "blocked" | "inflight" | "dead" | "completed";

// ── Theme Context ────────────────────────────────────────────────

const ThemeCtx = createContext<{ theme: Theme; toggle: () => void }>({
  theme: "crimson",
  toggle: () => {},
});

function useTheme() { return useContext(ThemeCtx); }

// ── Helpers ──────────────────────────────────────────────────────

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

function fuzzyMatch(query: string, text: string): boolean {
  if (!query) return true;
  const q = query.toLowerCase(), t = text.toLowerCase();
  let qi = 0;
  for (let i = 0; i < t.length && qi < q.length; i++) { if (t[i] === q[qi]) qi++; }
  return qi === q.length;
}

function stateColor(state: string): string {
  switch (state) {
    case "running": return "var(--green-bright)";
    case "starting": case "backoff": return "var(--gold)";
    case "failed": return "var(--accent-glow)";
    default: return "var(--text-3)";
  }
}

// ── Hooks ────────────────────────────────────────────────────────

function useSSE<T>(url: string | null, onMessage: (data: T) => void) {
  useEffect(() => {
    if (!url) return;
    const es = new EventSource(url);
    es.onmessage = (e) => { try { onMessage(JSON.parse(e.data)); } catch {} };
    return () => es.close();
  }, [url]); // eslint-disable-line react-hooks/exhaustive-deps
}

// ── App ──────────────────────────────────────────────────────────

export function App() {
  const [theme, setTheme] = useState<Theme>(() =>
    (localStorage.getItem("angl-theme") as Theme) || "crimson"
  );
  const toggle = useCallback(() => {
    setTheme((t) => {
      const next = t === "crimson" ? "azure" : "crimson";
      localStorage.setItem("angl-theme", next);
      return next;
    });
  }, []);

  useEffect(() => { document.documentElement.setAttribute("data-theme", theme); }, [theme]);

  const [section, setSection] = useState<Section>(() => {
    const hash = window.location.hash;
    if (hash.startsWith("#schedg")) return "schedg";
    return "angl";
  });

  return (
    <ThemeCtx.Provider value={{ theme, toggle }}>
      <div className="app-root" data-theme={theme}>
        <Header section={section} onSection={setSection} />
        <main className="app-main">
          {section === "angl" && <AnglSection />}
          {section === "schedg" && <SchedgSection />}
        </main>
      </div>
    </ThemeCtx.Provider>
  );
}

// ── Header ───────────────────────────────────────────────────────

function Header({ section, onSection }: { section: Section; onSection: (s: Section) => void }) {
  const { theme, toggle } = useTheme();
  return (
    <header className="hdr">
      <div className="hdr-left">
        <button className={`hdr-tab ${section === "angl" ? "hdr-tab-active" : ""}`} onClick={() => { onSection("angl"); window.history.replaceState(null, "", "#angl"); }}>
          <img src="/ornaments/beast.png" className="hdr-icon" alt="" />
          angl
        </button>
        <button className={`hdr-tab ${section === "schedg" ? "hdr-tab-active" : ""}`} onClick={() => { onSection("schedg"); window.history.replaceState(null, "", "#schedg"); }}>
          <img src="/ornaments/monastery.png" className="hdr-icon" alt="" />
          schedg
        </button>
      </div>
      <div className="hdr-right">
        <span className="live-dot">LIVE</span>
        <button className="theme-btn" onClick={toggle} title="Switch theme">
          {theme === "crimson" ? "crimson" : "azure"}
        </button>
        <span className="help-hint" onClick={() => document.dispatchEvent(new CustomEvent("toggle-help"))}>
          <kbd>?</kbd>
        </span>
      </div>
    </header>
  );
}

// ── Angl Section ─────────────────────────────────────────────────

function AnglSection() {
  const [angls, setAngls] = useState<AnglStatus[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [focusedIdx, setFocusedIdx] = useState(-1);
  const [tailLines, setTailLines] = useState<string[]>([]);
  const [helpVisible, setHelpVisible] = useState(false);
  const [stateFilter, setStateFilter] = useState<string | null>(null);
  const [detailMode, setDetailMode] = useState<"split" | "fullscreen">("split");
  const searchRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  useSSE<AnglStatus[]>("/api/angls/events", setAngls);

  useEffect(() => {
    if (!selected) { setTailLines([]); return; }
    const es = new EventSource(`/api/angls/${encodeURIComponent(selected)}/tail?history=200`);
    es.onmessage = (e) => setTailLines((prev) => [...prev.slice(-999), e.data]);
    return () => es.close();
  }, [selected]);

  const selectedAngl = useMemo(() => angls.find((a) => a.name === selected) ?? null, [angls, selected]);

  const filtered = useMemo(() => {
    let list = angls;
    if (stateFilter) list = list.filter((a) => a.state === stateFilter);
    if (!search) return list;
    return list.filter((a) => fuzzyMatch(search, a.name) || fuzzyMatch(search, a.charge ?? "") || (a.tags ?? []).some((t) => fuzzyMatch(search, t)));
  }, [angls, search, stateFilter]);

  const counts = useMemo(() => {
    const c: Record<string, number> = {};
    for (const a of angls) c[a.state] = (c[a.state] ?? 0) + 1;
    return c;
  }, [angls]);

  const selectAngl = useCallback((name: string) => { setSelected(name); setTailLines([]); setDetailMode("split"); }, []);
  const deselectAngl = useCallback(() => { setSelected(null); setTailLines([]); setDetailMode("split"); }, []);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const inInput = (e.target as HTMLElement)?.tagName === "INPUT";
      if (e.key === "?" && !inInput) { e.preventDefault(); setHelpVisible((v) => !v); return; }
      if (e.key === "Escape") { if (helpVisible) { setHelpVisible(false); return; } if (detailMode === "fullscreen") { setDetailMode("split"); return; } if (selected) { deselectAngl(); return; } return; }
      if (e.key === "/" && !inInput) { e.preventDefault(); searchRef.current?.focus(); return; }
      if (e.key === "Backspace" && !inInput) { e.preventDefault(); if (detailMode === "fullscreen") { setDetailMode("split"); return; } if (selected) { deselectAngl(); return; } return; }
      if (e.key === "f" && !inInput && selected) { e.preventDefault(); setDetailMode((m) => m === "split" ? "fullscreen" : "split"); return; }
      if (!inInput) {
        if (e.key === "j" || e.key === "ArrowDown") { e.preventDefault(); setFocusedIdx((i) => i < 0 ? 0 : Math.min(i + 1, filtered.length - 1)); return; }
        if (e.key === "k" || e.key === "ArrowUp") { e.preventDefault(); setFocusedIdx((i) => Math.max(i < 0 ? 0 : i - 1, 0)); return; }
        if (e.key === "Enter" && focusedIdx >= 0 && filtered[focusedIdx]) { e.preventDefault(); selectAngl(filtered[focusedIdx].name); return; }
        if (e.key === "c" && focusedIdx >= 0 && filtered[focusedIdx]) { copyText(filtered[focusedIdx].name); return; }
        const fk: Record<string, string | null> = { "1": "running", "2": "stopped", "3": "disabled", "4": "failed", "5": "backoff", "0": null };
        if (fk[e.key] !== undefined) { setStateFilter(fk[e.key]); setFocusedIdx(-1); return; }
      }
    };
    const helpToggle = () => setHelpVisible((v) => !v);
    document.addEventListener("toggle-help", helpToggle);
    window.addEventListener("keydown", handler);
    return () => { window.removeEventListener("keydown", handler); document.removeEventListener("toggle-help", helpToggle); };
  }, [helpVisible, filtered, focusedIdx, selected, detailMode, deselectAngl, selectAngl]);

  useEffect(() => setFocusedIdx(-1), [search, stateFilter]);
  useEffect(() => { listRef.current?.querySelector(".focused")?.scrollIntoView({ block: "nearest" }); }, [focusedIdx]);

  const listPanel = (
    <div className="list-panel">
      <div className="counts-bar">
        {(["running", "backoff", "stopped", "disabled", "failed"] as const).map((st, i) => (
          <button key={st} className={`count-btn ${stateFilter === st ? "active" : ""}`} onClick={() => { setStateFilter(stateFilter === st ? null : st); setFocusedIdx(-1); }}>
            <span className="count-key">{i + 1}</span>
            <span className="count-label">{st}</span>
            <span className={`count-num count-${st}`}>{counts[st] ?? 0}</span>
          </button>
        ))}
        <button className={`count-btn ${stateFilter === null ? "active" : ""}`} onClick={() => { setStateFilter(null); setFocusedIdx(-1); }}>
          <span className="count-key">0</span>
          <span className="count-label">all</span>
          <span className="count-num">{angls.length}</span>
        </button>
      </div>
      <SearchBar ref={searchRef} value={search} onChange={setSearch} count={filtered.length} placeholder="Filter angls... ( / )" />
      <div className="item-list" ref={listRef}>
        {filtered.length === 0 && <p className="empty">No angls{stateFilter ? ` in "${stateFilter}"` : ""}.</p>}
        {filtered.map((a, i) => (
          <div key={a.name} className={`item-row ${i === focusedIdx ? "focused" : ""} ${a.name === selected ? "selected" : ""}`} onClick={() => selectAngl(a.name)}>
            <span className="item-state" style={{ color: stateColor(a.state) }}>{a.state}</span>
            <span className="item-name">{a.name}</span>
            {a.interval && <span className="item-badge">{a.interval}</span>}
            {a.pid ? <span className="item-dim">pid {a.pid}</span> : null}
            {a.uptime && <span className="item-dim" style={{ color: "var(--green-bright)" }}>{a.uptime}</span>}
            <span className="item-desc">{a.charge ?? ""}</span>
            <CopyBtn text={a.name} />
          </div>
        ))}
      </div>
    </div>
  );

  return (
    <>
      {!selected && listPanel}
      {selected && (
        <div className="split">
          <div className="split-list">{listPanel}</div>
          <div className={`split-detail ${detailMode === "fullscreen" ? "fullscreen" : ""}`}>
            <DetailBar onBack={deselectAngl} mode={detailMode} onToggle={() => setDetailMode((m) => m === "split" ? "fullscreen" : "split")} />
            {selectedAngl && <AnglInfo angl={selectedAngl} />}
            <h3 className="section-sub">Live Tail</h3>
            <TailView lines={tailLines} />
          </div>
        </div>
      )}
      {helpVisible && <HelpOverlay onClose={() => setHelpVisible(false)} keys={[
        ["?", "Toggle help"], ["j / k", "Navigate"], ["Enter", "Open"], ["Backspace", "Back"],
        ["/", "Search"], ["1-5, 0", "Filter state"], ["c", "Copy name"], ["f", "Fullscreen"], ["Esc", "Close"],
      ]} />}
    </>
  );
}

function AnglInfo({ angl }: { angl: AnglStatus }) {
  return (
    <div className="detail-info">
      <div className="detail-header">
        <span className="detail-id">{angl.name}</span>
        <span className="badge" style={{ borderColor: stateColor(angl.state), color: stateColor(angl.state) }}>{angl.state}</span>
      </div>
      {angl.charge && <div className="detail-charge">{angl.charge}</div>}
      <div className="meta-grid">
        {angl.pid ? <MetaRow k="PID" v={String(angl.pid)} /> : null}
        {angl.uptime && <MetaRow k="Uptime" v={angl.uptime} />}
        {angl.interval && <MetaRow k="Interval" v={angl.interval} />}
        {angl.started && <MetaRow k="Started" v={relativeTime(angl.started)} />}
        <MetaRow k="Restarts" v={angl.max_restarts ? `${angl.restarts}/${angl.max_restarts}` : String(angl.restarts)} />
        {angl.lifetime && <MetaRow k="Lifetime" v={angl.lifetime} />}
      </div>
    </div>
  );
}

// ── Schedg Section ───────────────────────────────────────────────

const TABS: ListTab[] = ["ready", "blocked", "inflight", "dead", "completed"];
const TAB_LABELS: Record<ListTab, string> = { ready: "Ready", blocked: "Blocked", inflight: "In-Flight", dead: "Dead", completed: "Completed" };

function SchedgSection() {
  const [queues, setQueues] = useState<QueueEntry[]>([]);
  const [selectedQueue, setSelectedQueue] = useState<string | null>(null);
  const [snapshot, setSnapshot] = useState<QueueSnapshot | null>(null);
  const [activeTab, setActiveTab] = useState<ListTab>("ready");
  const [search, setSearch] = useState("");
  const [focusedIdx, setFocusedIdx] = useState(-1);
  const [selectedTask, setSelectedTask] = useState<TaskView | null>(null);
  const [detailMode, setDetailMode] = useState<"split" | "fullscreen">("split");
  const [helpVisible, setHelpVisible] = useState(false);
  const [view, setView] = useState<"list" | "queue" | "graph">("list");
  const searchRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => { fetch("/api/queues").then((r) => r.json()).then(setQueues).catch(() => {}); }, []);

  const sseUrl = selectedQueue && view !== "list" ? `/api/queues/${encodeURIComponent(selectedQueue)}/events` : null;
  useSSE<QueueSnapshot>(sseUrl, setSnapshot);

  const currentTasks = useMemo(() => {
    if (!snapshot) return [];
    const lists: Record<ListTab, TaskView[]> = { ready: snapshot.ready, blocked: snapshot.blocked, inflight: snapshot.inflight, dead: snapshot.dead, completed: snapshot.completed };
    const all = lists[activeTab] || [];
    if (!search) return all;
    return all.filter((t) => fuzzyMatch(search, t.title) || fuzzyMatch(search, t.id));
  }, [snapshot, activeTab, search]);

  const selectQueue = useCallback((name: string) => { setSelectedQueue(name); setView("queue"); setActiveTab("ready"); setSearch(""); setFocusedIdx(-1); setSelectedTask(null); }, []);
  const goBack = useCallback(() => {
    if (selectedTask) { setSelectedTask(null); setDetailMode("split"); }
    else if (view === "graph") { setView("queue"); }
    else { setView("list"); setSelectedQueue(null); setSnapshot(null); }
  }, [selectedTask, view]);

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const inInput = (e.target as HTMLElement)?.tagName === "INPUT";
      if (e.key === "?" && !inInput) { e.preventDefault(); setHelpVisible((v) => !v); return; }
      if (e.key === "Escape") { if (helpVisible) { setHelpVisible(false); return; } if (detailMode === "fullscreen") { setDetailMode("split"); return; } return; }
      if (e.key === "/" && !inInput) { e.preventDefault(); searchRef.current?.focus(); return; }
      if (e.key === "Backspace" && !inInput) { e.preventDefault(); if (detailMode === "fullscreen") { setDetailMode("split"); return; } goBack(); return; }
      if (e.key === "f" && !inInput && selectedTask) { e.preventDefault(); setDetailMode((m) => m === "split" ? "fullscreen" : "split"); return; }
      if (e.key === "g" && !inInput && view === "queue") { e.preventDefault(); setView("graph"); return; }
      if (!inInput) {
        const max = view === "list" ? queues.length : currentTasks.length;
        if (e.key === "j" || e.key === "ArrowDown") { e.preventDefault(); setFocusedIdx((i) => i < 0 ? 0 : Math.min(i + 1, max - 1)); return; }
        if (e.key === "k" || e.key === "ArrowUp") { e.preventDefault(); setFocusedIdx((i) => Math.max(i < 0 ? 0 : i - 1, 0)); return; }
        if (e.key === "Enter" && focusedIdx >= 0) {
          e.preventDefault();
          if (view === "list" && queues[focusedIdx]) selectQueue(queues[focusedIdx].name);
          else if (view === "queue" && currentTasks[focusedIdx]) setSelectedTask(currentTasks[focusedIdx]);
          return;
        }
        if (view === "queue") {
          const tk: Record<string, ListTab> = { "1": "ready", "2": "blocked", "3": "inflight", "4": "dead", "5": "completed" };
          if (tk[e.key]) { setActiveTab(tk[e.key]); setFocusedIdx(-1); return; }
        }
      }
    };
    const helpToggle = () => setHelpVisible((v) => !v);
    document.addEventListener("toggle-help", helpToggle);
    window.addEventListener("keydown", handler);
    return () => { window.removeEventListener("keydown", handler); document.removeEventListener("toggle-help", helpToggle); };
  }, [helpVisible, view, queues, currentTasks, focusedIdx, selectedTask, detailMode, goBack, selectQueue]);

  useEffect(() => setFocusedIdx(-1), [activeTab, search]);
  useEffect(() => { listRef.current?.querySelector(".focused")?.scrollIntoView({ block: "nearest" }); }, [focusedIdx]);

  const queueListPanel = (
    <div className="list-panel">
      <h2 className="section-title">Registered Queues</h2>
      {queues.length === 0 && <p className="empty">No queues registered.</p>}
      {queues.map((q, i) => (
        <div key={q.name} className={`item-row queue-card ${i === focusedIdx ? "focused" : ""}`} onClick={() => selectQueue(q.name)}>
          <img src={`/ornaments/square${(i % 5) + 1}.png`} className="ornament-card" alt="" />
          <span className="item-name">{q.name}</span>
          <span className="item-badge">{q.driver}</span>
          <span className="item-dim">{q.path}</span>
        </div>
      ))}
    </div>
  );

  const taskListPanel = snapshot && (
    <div className="list-panel">
      <div className="counts-bar">
        {TABS.map((tab, i) => (
          <button key={tab} className={`count-btn ${activeTab === tab ? "active" : ""}`} onClick={() => { setActiveTab(tab); setFocusedIdx(-1); }}>
            <span className="count-key">{i + 1}</span>
            <span className="count-label">{TAB_LABELS[tab]}</span>
            <span className={`count-num count-${tab}`}>{snapshot.counts[tab] ?? 0}</span>
          </button>
        ))}
        <button className="count-btn graph-btn" onClick={() => setView("graph")} title="Dep graph (g)">
          <span className="count-label">Deps</span>
          <span className="count-num">{snapshot.deps.length}</span>
        </button>
      </div>
      <SearchBar ref={searchRef} value={search} onChange={setSearch} count={currentTasks.length} placeholder="Filter tasks... ( / )" />
      <div className="item-list" ref={listRef}>
        {currentTasks.length === 0 && <p className="empty">No tasks in {TAB_LABELS[activeTab].toLowerCase()}</p>}
        {currentTasks.map((t, i) => (
          <div key={t.id} className={`item-row state-${activeTab} ${i === focusedIdx ? "focused" : ""}`} onClick={() => setSelectedTask(t)}>
            <span className="item-id">#{t.id}</span>
            <span className={`prio p${Math.min(t.priority, 9)}`}>p{t.priority}</span>
            <span className="item-name">{t.title || `Task #${t.id}`}</span>
            {t.leasedAt && <span className="item-dim" style={{ color: "var(--accent-bright)" }}>{relativeTime(t.leasedAt)}</span>}
            {(t.kv && Object.keys(t.kv).length > 0) && <span className="item-badge">{Object.keys(t.kv).length} kv</span>}
            <CopyBtn text={t.title || t.id} />
          </div>
        ))}
      </div>
    </div>
  );

  return (
    <>
      {view === "list" && queueListPanel}
      {view === "queue" && snapshot && !selectedTask && taskListPanel}
      {view === "queue" && snapshot && selectedTask && (
        <div className="split">
          <div className="split-list">{taskListPanel}</div>
          <div className={`split-detail ${detailMode === "fullscreen" ? "fullscreen" : ""}`}>
            <DetailBar onBack={() => setSelectedTask(null)} mode={detailMode} onToggle={() => setDetailMode((m) => m === "split" ? "fullscreen" : "split")} />
            <TaskDetail task={selectedTask} snapshot={snapshot} />
          </div>
        </div>
      )}
      {view === "graph" && snapshot && <DepGraph snapshot={snapshot} onSelect={setSelectedTask} onBack={() => setView("queue")} />}
      {helpVisible && <HelpOverlay onClose={() => setHelpVisible(false)} keys={[
        ["?", "Toggle help"], ["j / k", "Navigate"], ["Enter", "Open"], ["Backspace", "Back"],
        ["/", "Search"], ["1-5", "Switch tab"], ["g", "Dep graph"], ["f", "Fullscreen"], ["Esc", "Close"],
      ]} />}
    </>
  );
}

// ── Task Detail ──────────────────────────────────────────────────

function TaskDetail({ task, snapshot }: { task: TaskView; snapshot: QueueSnapshot }) {
  const state = snapshot.inflight.find((t) => t.id === task.id) ? "inflight"
    : snapshot.dead.find((t) => t.id === task.id) ? "dead"
    : snapshot.blocked.find((t) => t.id === task.id) ? "blocked"
    : snapshot.completed.some((t) => t.id === task.id) ? "completed" : "ready";
  const blockedBy = snapshot.blockedBy?.[task.id] || [];

  return (
    <div className="detail-info">
      <div className="detail-header">
        <span className="detail-id">#{task.id}</span>
        <span className={`badge badge-${state}`}>{state}</span>
        <span className={`prio p${Math.min(task.priority, 9)}`}>p{task.priority}</span>
        <CopyBtn text={`#${task.id} ${task.title}\n\n${task.description || ""}`} />
      </div>
      <h2 className="detail-title">{task.title}</h2>
      {task.description && (
        <div className="detail-body">
          <div className="detail-md"><ReactMarkdown remarkPlugins={[remarkGfm]}>{task.description}</ReactMarkdown></div>
          <CopyBtn text={task.description} className="copy-body" />
        </div>
      )}
      <div className="meta-grid">
        {task.submitted && <MetaRow k="Submitted" v={`${task.submitted} (${relativeTime(task.submitted)})`} />}
        {task.leasedAt && <MetaRow k="Leased At" v={`${task.leasedAt} (${relativeTime(task.leasedAt)})`} />}
        {(task.attempts ?? 0) > 0 && <MetaRow k="Attempts" v={String(task.attempts)} />}
        {(task.cancels ?? 0) > 0 && <MetaRow k="Cancels" v={String(task.cancels)} />}
        {task.reason && <MetaRow k="Reason" v={task.reason} accent />}
        {blockedBy.length > 0 && <MetaRow k="Blocked By" v={blockedBy.map((id) => `#${id}`).join(", ")} />}
      </div>
      {task.kv && Object.keys(task.kv).length > 0 && (
        <div className="kv-section">
          <h3 className="kv-heading">Annotations</h3>
          {Object.entries(task.kv).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => (
            <div key={k} className="kv-entry">
              <div className="kv-key">{k} <CopyBtn text={v} /></div>
              <div className="kv-val"><ReactMarkdown remarkPlugins={[remarkGfm]}>{v.length > 200 ? v.slice(0, 200) + "..." : v}</ReactMarkdown></div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// ── Dep Graph ────────────────────────────────────────────────────

function DepGraph({ snapshot, onSelect, onBack }: { snapshot: QueueSnapshot; onSelect: (t: TaskView) => void; onBack: () => void }) {
  const allTasks = useMemo(() => {
    const map = new Map<string, TaskView>();
    for (const t of [...snapshot.ready, ...snapshot.blocked, ...snapshot.inflight, ...snapshot.dead]) map.set(t.id, t);
    return map;
  }, [snapshot]);

  const nodeIds = useMemo(() => {
    const s = new Set<string>();
    for (const e of snapshot.deps) { s.add(e.from); s.add(e.to); }
    if (s.size === 0) for (const t of snapshot.ready) s.add(t.id);
    return Array.from(s);
  }, [snapshot]);

  const positions = useMemo(() => {
    const pos = new Map<string, { x: number; y: number }>();
    const inDeg = new Map<string, number>(), outEdges = new Map<string, string[]>();
    for (const id of nodeIds) { inDeg.set(id, 0); outEdges.set(id, []); }
    for (const e of snapshot.deps) { inDeg.set(e.from, (inDeg.get(e.from) ?? 0) + 1); outEdges.get(e.to)?.push(e.from); }
    const layers: string[][] = [], visited = new Set<string>();
    let current = nodeIds.filter((id) => (inDeg.get(id) ?? 0) === 0);
    while (current.length > 0) { layers.push(current); current.forEach((id) => visited.add(id)); const next: string[] = []; for (const id of current) for (const child of outEdges.get(id) ?? []) if (!visited.has(child)) { inDeg.set(child, (inDeg.get(child) ?? 0) - 1); if ((inDeg.get(child) ?? 0) <= 0) next.push(child); } current = next; }
    const placed = new Set(layers.flat()); const remaining = nodeIds.filter((id) => !placed.has(id)); if (remaining.length > 0) layers.push(remaining);
    for (let li = 0; li < layers.length; li++) for (let ni = 0; ni < layers[li].length; ni++) pos.set(layers[li][ni], { x: 60 + li * 220, y: 40 + ni * 60 });
    return pos;
  }, [nodeIds, snapshot.deps]);

  const svgW = Math.max(600, (positions.size > 0 ? Math.max(...Array.from(positions.values()).map((p) => p.x)) : 0) + 200);
  const svgH = Math.max(300, (positions.size > 0 ? Math.max(...Array.from(positions.values()).map((p) => p.y)) : 0) + 80);

  const stateOf = (id: string) => {
    if (snapshot.inflight.find((t) => t.id === id)) return "inflight";
    if (snapshot.dead.find((t) => t.id === id)) return "dead";
    if (snapshot.blocked.find((t) => t.id === id)) return "blocked";
    if (snapshot.completed.some((t) => t.id === id)) return "completed";
    return "ready";
  };

  return (
    <div>
      <button className="back-btn" onClick={onBack}>&larr; Back to queue</button>
      <div className="graph-container">
        <svg width={svgW} height={svgH}>
          <defs><marker id="arrow" viewBox="0 0 10 10" refX="10" refY="5" markerWidth="8" markerHeight="8" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="var(--text-3)" /></marker></defs>
          {snapshot.deps.map((e, i) => { const from = positions.get(e.from), to = positions.get(e.to); if (!from || !to) return null; return <line key={i} x1={to.x + 80} y1={to.y + 16} x2={from.x} y2={from.y + 16} stroke="var(--text-3)" strokeWidth={1.5} markerEnd="url(#arrow)" />; })}
          {nodeIds.map((id) => { const p = positions.get(id); if (!p) return null; const task = allTasks.get(id); const state = stateOf(id); return (
            <g key={id} transform={`translate(${p.x},${p.y})`} className={`graph-node gn-${state}`} onClick={() => task && onSelect(task)} style={{ cursor: task ? "pointer" : "default" }}>
              <rect width={160} height={32} rx={4} />
              <text x={8} y={20} className="graph-label">#{id} {task?.title ? task.title.slice(0, 18) : ""}</text>
            </g>
          ); })}
        </svg>
      </div>
    </div>
  );
}

// ── Shared Components ────────────────────────────────────────────

import { forwardRef } from "react";

const SearchBar = forwardRef<HTMLInputElement, { value: string; onChange: (v: string) => void; count: number; placeholder: string }>(
  ({ value, onChange, count, placeholder }, ref) => (
    <div className="search-row">
      <input ref={ref} className="search" type="text" placeholder={placeholder} value={value} onChange={(e) => onChange(e.target.value)} spellCheck={false} />
      <span className="search-count">{count}</span>
    </div>
  )
);

function CopyBtn({ text, className }: { text: string; className?: string }) {
  return <button className={`copy-btn ${className ?? ""}`} title="Copy" onClick={(e) => { e.stopPropagation(); copyText(text); }}>&#x2398;</button>;
}

function MetaRow({ k, v, accent }: { k: string; v: string; accent?: boolean }) {
  return (
    <div className="meta-row">
      <span className="meta-key">{k}</span>
      <span className={`meta-val ${accent ? "meta-accent" : ""}`}>{v}</span>
    </div>
  );
}

function DetailBar({ onBack, mode, onToggle }: { onBack: () => void; mode: "split" | "fullscreen"; onToggle: () => void }) {
  return (
    <div className="detail-bar">
      <button className="back-btn" onClick={onBack}>&larr; Back</button>
      <button className="fullscreen-btn" onClick={onToggle}>{mode === "fullscreen" ? "[=] Split" : "[ ] Fullscreen"}</button>
    </div>
  );
}

function TailView({ lines }: { lines: string[] }) {
  const endRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [autoScroll, setAutoScroll] = useState(true);
  useEffect(() => { if (autoScroll) endRef.current?.scrollIntoView({ behavior: "smooth" }); }, [lines.length, autoScroll]);
  const handleScroll = useCallback(() => { const el = containerRef.current; if (!el) return; setAutoScroll(el.scrollHeight - el.scrollTop - el.clientHeight < 40); }, []);
  return (
    <div className="tail" ref={containerRef} onScroll={handleScroll}>
      {lines.length === 0 && <p className="empty">No log output yet.</p>}
      {lines.map((line, i) => <div key={i} className={`tail-line ${line.includes("ERROR") || line.includes("error") ? "tail-error" : line.includes("warning") ? "tail-warn" : ""}`}>{line}</div>)}
      <div ref={endRef} />
    </div>
  );
}

function HelpOverlay({ onClose, keys }: { onClose: () => void; keys: string[][] }) {
  return (
    <div className="overlay" onClick={onClose}>
      <div className="help" onClick={(e) => e.stopPropagation()}>
        <div className="help-header">
          <img src="/ornaments/castle.png" className="ornament-help" alt="" />
          <h2>Keyboard Shortcuts</h2>
        </div>
        <table><tbody>
          {keys.map(([key, label]) => <tr key={key}><td><kbd>{key}</kbd></td><td>{label}</td></tr>)}
        </tbody></table>
      </div>
    </div>
  );
}
