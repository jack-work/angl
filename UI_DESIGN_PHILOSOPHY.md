# UI Design Philosophy: Techno-Medieval Manuscript Interface

**READ THIS BEFORE MAKING ANY UI CHANGES.** The aesthetic was built through deliberate research and iteration, not generated from defaults. Every font, texture, and ornament was chosen with intent. Deviating without understanding the source material will produce incoherent results.

## Core Concept

Industrial-utilitarian dashboard meets illuminated manuscript. The tension between these two poles is the entire point: modern data-dense UX (keyboard-driven, split-pane, SSE live updates, fuzzy search) dressed in the visual language of medieval scriptoria. It should feel like a monk built a priority queue tracker in a candlelit workshop, but the monk also knows vim keybindings.

NOT maximalist fantasy decoration. NOT generic dark-mode SaaS. The restraint matters as much as the ornamentation. Ornaments are sparse, small, and low-opacity. The parchment texture is barely visible through a heavy dark overlay. The gothic dropcaps only appear on the first letter of markdown body text. The floral border is 10px tall at 20% opacity. If you can't tell it's there at a glance, that's correct.

## Dual Theme System

Two themes share the same layout, components, and fonts. Only CSS custom properties change:

- **Crimson** (warm): `#7a1518` / `#a82024` / `#c93030` accent hierarchy. Warm browns and parchment tones for backgrounds (`#1a150f` through `#352b1f`). Text in aged-paper beiges (`#e4d5b7`, `#c4b08a`). Gold (`#b8963a`) for secondary accents, keys, tags. Dark overlay at 88% over parchment texture.

- **Azure** (cool): `#1e4876` / `#2a6aaa` / `#3a8ad0` accent hierarchy. Cool blue-grays for backgrounds (`#0e1118` through `#232938`). Text in steel blues (`#d4dae8`, `#a0aac0`). Same gold for secondary. Dark overlay at 92% over parchment.

The themes toggle via a `[data-theme]` attribute on the root element. Only `--accent`, `--accent-bright`, `--accent-glow`, background colors, text colors, overlay opacity, and selection color change. Everything else (fonts, ornaments, layout, spacing) stays identical. Stored in `localStorage("angl-theme")`.

## Typography Stack (6 fonts, each with a specific role)

### Display / Headings

1. **P22 Morris Troy** (`--morris`): Logo text, section titles, help dialog headings. A William Morris / Kelmscott Press revival. Heavy, ornamental, deeply medieval. Used ONLY for short display text (1-3 words). Never for body copy.
   - Local asset: `C:/Users/jokellih/dev/angl/web/public/morris-troy.ttf`
   - Source: https://saintsoftheisles.neocities.org/P22%20Morris%20Troy%20Regular.ttf

2. **P22 Morris Golden** (`--golden`): Queue/angl names, task detail titles. The companion body face to Morris Troy. More readable at smaller sizes but still distinctly Arts & Crafts.
   - Local asset: `C:/Users/jokellih/dev/angl/web/public/morris-golden.ttf`
   - Source: https://saintsoftheisles.neocities.org/P22%20Morris%20Golden%20Regular.ttf

3. **Cinzel / Cinzel Decorative** (`--display` / `--display-deco`): Uppercase labels, tab keys, meta row keys, badge text, breadcrumbs, back buttons. A classicist Roman inscription face. Used for small ALL-CAPS labels with wide letter-spacing (1-3px). Loaded from Google Fonts.
   - Google Fonts: `family=Cinzel:wght@400;500;600;700;900&family=Cinzel+Decorative:wght@400;700;900`

### Body / Reading

4. **IM Fell English** (`--serif`): All body text, task titles in list rows, description prose, help dialog descriptions, search placeholders (italic). An actual digitization of an early 18th-century English typeface. Gives warmth and historical texture to readable text. Loaded from Google Fonts.
   - Google Fonts: `family=IM+Fell+English:ital@0;1`

### Decorative

5. **UnifrakturCook** (`--gothic`): Gothic/Fraktur blackletter. Used ONLY for: the dropcap `::first-letter` on description body text (3.5em, floated left), and the detail-view `#id` number (26px). Maximum 1-2 characters at a time. Never for words. Loaded from Google Fonts.
   - Google Fonts: `family=UnifrakturCook:wght@700`

### Data / Code

6. **JetBrains Mono** (`--mono`): All code, IDs, timestamps, priority badges, KV keys, search input, tail log lines, count numbers. The modern technical counterpoint to all the historical faces. Loaded from Google Fonts.
   - Google Fonts: `family=JetBrains+Mono:wght@400;500;600;700`

## Texture & Background

The entire page sits on a seamless parchment tile with a very heavy dark overlay (88-92% opacity). The parchment is barely perceptible but creates depth that a flat solid color cannot.

- **Parchment tile** (1920x1920, JPEG, seamless repeat at 400px):
  - Local: `C:/Users/jokellih/dev/angl/web/public/parchment.jpg`
  - Source: https://secret-manuscript.neocities.org/seamless.jpg

- **Paper texture** (2000x1417, PNG, from a medieval manuscript site):
  - Local: `C:/Users/jokellih/dev/angl/web/public/paper-texture.png`
  - Source: https://blamensir.neocities.org/img/paper-texture-1184163.png

## Ornamental Assets (manuscript illustrations, borders, knotwork)

All sourced from neocities sites hosting medieval manuscript recreations. Used at small sizes (10-40px), low opacity (15-50%), often with `filter: sepia()` and `brightness()` to blend with the dark theme.

### Celtic knotwork tiles (Saints of the Isles)
Each ~130x125px, illuminated manuscript-style square panels:
- `C:/Users/jokellih/dev/angl/web/public/ornaments/square1.png` - interlaced beasts
- `C:/Users/jokellih/dev/angl/web/public/ornaments/square2.png` - Celtic cross knot
- `C:/Users/jokellih/dev/angl/web/public/ornaments/square3.png` - geometric floral
- `C:/Users/jokellih/dev/angl/web/public/ornaments/square4.png` - knotwork panel
- `C:/Users/jokellih/dev/angl/web/public/ornaments/square5.png` - interlace pattern
- Source: https://saintsoftheisles.neocities.org/sqaure1.png (through sqaure5.png, note the original misspelling)
- Usage: queue card icons (20px, 35% opacity, cycles through 1-5)

### Floral vine border (Saints of the Isles)
1920x66px, repeating vine scroll ornament:
- `C:/Users/jokellih/dev/angl/web/public/ornaments/floral-border.png`
- Source: https://saintsoftheisles.neocities.org/blue%20line.png
- Usage: header bottom border (10px, 20% opacity, repeat-x), section title underlines, description body top edge, help dialog top border

### Manuscript illustrations (Secret Manuscript)
Individual illuminated drawings, transparent backgrounds:
- `C:/Users/jokellih/dev/angl/web/public/ornaments/beast.png` (241x280) - rabbit playing trumpet. **The mascot.** Used as favicon and logo icon.
  - Source: https://secret-manuscript.neocities.org/beaft.png
- `C:/Users/jokellih/dev/angl/web/public/ornaments/lighthouse.png` (75x320) - tower with golden light
  - Source: https://secret-manuscript.neocities.org/lighthouse.png
  - Usage: LIVE indicator icon (16px)
- `C:/Users/jokellih/dev/angl/web/public/ornaments/monastery.png` (280x220) - red-roofed monastery
  - Source: https://secret-manuscript.neocities.org/monastery.png
  - Usage: schedg tab icon in header (22px)
- `C:/Users/jokellih/dev/angl/web/public/ornaments/castle.png` (240x280) - medieval castle with blue roofs
  - Source: https://secret-manuscript.neocities.org/castlecrop.png
  - Usage: help dialog header ornament (40px)

### Botanical illustrations (Blamensir)
Medieval herbal manuscript style, large originals:
- `C:/Users/jokellih/dev/angl/web/public/ornaments/plant.png` - rosemary-like herb with roots
  - Source: https://blamensir.neocities.org/img/plant.png
- `C:/Users/jokellih/dev/angl/web/public/ornaments/baldrian.png` - valerian plant
  - Source: https://blamensir.neocities.org/img/baldrian.png
- `C:/Users/jokellih/dev/angl/web/public/ornaments/wegerich.png` - plantain herb
  - Source: https://blamensir.neocities.org/img/wegerich.png
- `C:/Users/jokellih/dev/angl/web/public/ornaments/quendel.png` - thyme plant
  - Source: https://blamensir.neocities.org/img/quendel.png
- Currently unused in UI but available for new panels/sections

### Celtic half-rounds (Saints of the Isles)
Quarter-circle illuminated corner pieces (~155x160px):
- `C:/Users/jokellih/dev/angl/web/public/ornaments/half-left.png`
- `C:/Users/jokellih/dev/angl/web/public/ornaments/half-right.png`
- Source: https://saintsoftheisles.neocities.org/half%20left.png (and right)
- Removed from active use (occluded content) but available for careful placement

### Kanzlei illuminated alphabet (Gwern)
1600x654px specimen sheet of all 26 letters in Kanzlei blackletter with flourished vine borders:
- `C:/Users/jokellih/dev/angl/web/public/ornaments/kanzlei.png`
- Source: https://gwern.net/doc/design/typography/dropcap/kanzleiinitialen.png
- Context: https://gwern.net/dropcap#kanzlei
- Not currently used as individual letters but could be cropped for custom dropcaps

## Reference Sites

Study these before making design decisions:

1. **https://secret-manuscript.neocities.org/** - The parchment tile source. Simple layout, manuscript map feel. Shows how seamless.jpg tiles.

2. **https://saintsoftheisles.neocities.org/** - Source for knotwork tiles, vine border, Morris fonts, corner pieces. Study how they use the ornaments as structural elements (section headers, navigation borders) rather than scattered decoration.

3. **https://blamensir.neocities.org/hills#foothills** - Source for botanical illustrations. Shows layered medieval landscape with paper textures. The paper-texture.png background originates here.

4. **https://bestiary.ca/** - Referenced for its parchment background treatment. Clean, scholarly medieval aesthetic. Notice the restraint.

5. **https://gwern.net/dropcap#kanzlei** - Source for the Kanzlei alphabet specimen. Study his dropcap implementation (CSS `::first-letter` with historical typefaces). His entire site is a masterclass in typographic care.

## Layout Principles

- **No border-radius > 2px.** Sharp edges throughout. Manuscripts don't have rounded corners.
- **Left-border accents** (3px) on list rows and detail bodies. Color codes state (green=ready, gold=blocked, blue=inflight, accent=dead).
- **No chrome.** No header bar, no top navigation, no sidebar. The entire viewport is the pane canvas. All global actions go through the command palette or hotkeys.
- **Keyboard-first.** Every action has a shortcut. The mouse is a fallback. j/k navigation, / for search, 1-5 for tabs, Enter to open, Backspace to go back.
- **SSE for liveness.** Both angl process list and schedg queue snapshots push via EventSource every 2 seconds. The green LIVE dot pulses to confirm.
- **Copy buttons on everything.** Every piece of text the user might want to extract gets a subtle copy button (the &#x2398; character, styled as a button that turns gold on hover).
- **Column-aligned list fields.** State, name, badges, and descriptions in list rows must occupy fixed or min-width columns so fields align vertically across rows. Never let flexbox push fields to random positions.
- **Subtle breadcrumbs.** The back button in detail views is a tiny mono `<-` at 50% opacity inline with the name and badges. It should not dominate the layout or create negative space.

## Tiling Pane System

The viewport is a recursive binary tree of panes. No header, no sidebar -- just panes. The user builds their workspace by splitting, closing, and swapping panes to suit their task.

### Pane tree structure

```
PaneNode = Leaf { id, view }
         | Split { id, direction: h|v, ratio: 0-1, a: PaneNode, b: PaneNode }
```

Each leaf holds one of four view types:
- **angl-list**: filterable list of all angls with state badges, fuzzy search, keyboard nav
- **schedg-list**: filterable list of all registered schedg queues
- **angl-detail**: metadata grid, message input, live tail, in-flight message below
- **schedg-detail**: tab bar (ready/blocked/inflight/dead/completed), search, expandable task items

### Constraints

- Maximum 8 leaf panes (2 rows x 4 columns equivalent)
- Minimum pane ratio 10% (can't squeeze a pane to nothing)
- Drag-resizable handles between splits (4px, accent color on hover)
- Focused pane has an accent border; all pane-scoped hotkeys apply to the focused pane

### Pane hotkeys

| Key | Action |
|-----|--------|
| `-` | Split focused pane horizontally (below) |
| `\|` | Split focused pane vertically (right) |
| `Ctrl+W` | Close focused pane (won't close last) |
| `Ctrl+h/j/k/l` | Move focus to adjacent pane (spatial, by DOM bounding rect) |
| `Tab` / `Shift+Tab` | Cycle focus sequentially through leaf order |
| `Ctrl+R` | Toggle resize mode (hjkl adjusts split ratios, Esc exits) |
| `Ctrl+X` | Toggle swap mode (hjkl picks neighbor to swap views with, Esc exits) |

### Opening items in panes

When a list item is focused:
- `Enter` or `*` opens in the current pane (replaces the list with the detail view)
- `l` opens in a new split to the right
- `s` opens in a new split below
- Middle-click opens in a new split to the right

### Cross-linking

Angl detail views contain links to their associated queues:
- A "queue ->" button in the top bar opens the angl's per-message schedg (`angl.<name>`) in a split
- `schedg:*` tags are clickable and open the referenced work queue in a split
- The in-flight message section has an "all messages ->" link to the full queue view

### Right-click context menu

Right-clicking any pane opens a context menu with: Split Right, Split Below, switch view (Angl List / Queue List), Resize Mode, Swap Mode, Command Mode, Close Pane.

## Command Palette

`:` opens a vim-style command palette that is the primary interface for all global actions. It floats centered at ~15vh from top, 480px wide, with a dark backdrop blur.

### Architecture

Commands are defined declaratively. Each command has:
- **name**: the primary invocation word
- **aliases**: short forms (e.g., `q` for `close`, `o` for `open`)
- **args**: argument description string for display
- **desc**: human-readable description
- **completionCtx**: names a completion source for the argument

Completion sources are served by the daemon via a `/api/completions?context=<name>` endpoint, which proxies to the daemon's `completions` RPC method. This ensures the web UI, CLI, and any future client share the same completion data.

### Available completion contexts

| Context | Source | Returns |
|---------|--------|---------|
| `angls` | Daemon process list | All registered angls with charge as detail |
| `queues` | schedg config | All registered schedg queues with driver as detail |
| `themes` | Static | `crimson`, `azure` |
| `views` | Static | `angls`, `queues` |
| `directions` | Static | `h`, `v` |
| `states` | Static | `running`, `backoff`, `stopped`, `disabled`, `failed` |

### Command list

#### Pane commands
| Command | Args | Description |
|---------|------|-------------|
| `split` | `h\|v` | Split focused pane |
| `vsplit` | | Split right |
| `hsplit` | | Split below |
| `close` / `q` | | Close focused pane |
| `view` | `angls\|queues` | Switch pane view type |
| `open` / `o` | `<name>` | Open an angl or queue by name (fuzzy match) |
| `queue` | `<name>` | Open a queue specifically |
| `focus` | `h\|j\|k\|l\|next\|prev` | Move focus |
| `resize` | | Enter resize mode |
| `swap` | | Enter swap mode |

#### Daemon proxy commands
| Command | Args | Description |
|---------|------|-------------|
| `start` | `<angl>` | Start an angl (proxies daemon RPC) |
| `stop` | `<angl>` | Stop an angl |
| `enable` | `<angl>` | Enable an angl |
| `disable` | `<angl>` | Disable an angl |
| `restart` | `<angl>` | Restart an angl |
| `message` / `msg` | `<angl> <text>` | Send a message to an angl's schedg queue (wakes it) |
| `status` | `<angl>` | Open angl detail view |

#### UI commands
| Command | Args | Description |
|---------|------|-------------|
| `theme` | `crimson\|azure` | Switch color theme |
| `help` / `?` | | List all commands |

### Interaction

- `Tab` completes the selected suggestion into the input
- `Arrow Up/Down` or `Ctrl+n/p` navigates the completion list
- `Enter` executes the selected completion (or the raw input if no match)
- `Esc` dismisses the palette
- The colon prompt (`:`) is styled in `--accent-bright`, mono 16px, bold

### Design intent

The command palette is the spiritual equivalent of the ex command line in vi. It is the single point of control for the entire interface. Every button in the UI is a convenience alias for a command. If the command palette were the only interaction method, the interface would still be fully functional. This is intentional -- the palette makes the interface scriptable and discoverable without requiring memorization of hotkeys.

## Angl Detail View

The detail view is the primary workspace for monitoring a single angl. Layout from top to bottom:

1. **Top bar** (compact, single line): back arrow, name in golden font, state badge, interval badge, next-run countdown, "queue ->" link. No vertical padding waste.
2. **Charge** (italic serif, muted, one line)
3. **Tags** (left-aligned row of tag badges; `schedg:*` tags are clickable links)
4. **Meta grid** (key-value pairs: PID, Uptime, Started, Last Exit, Restarts, Lifetime)
5. **Message input** (mono text input + "send" button, Enter submits, sends via daemon RPC)
6. **Live Tail** (mono 10px, SSE-streamed log lines, auto-scroll with manual override)
7. **In-flight message** (below tail, only shown if a message is currently in-flight; blue left-border, badge, markdown-rendered description, "all messages ->" link)

## Schedg Detail View

1. **Top bar** (compact): back arrow, queue name, active count
2. **Tab bar** (ready/blocked/inflight/dead/completed, with count badges)
3. **Search** (fuzzy filter on title, id, description)
4. **Task list** (expandable items):
   - Each row shows: id, priority badge, title, caller badge, leased time
   - `Enter` or `x` toggles expand/collapse in place
   - Expanded view shows full markdown description + metadata grid
   - All markdown is rendered with themed link colors (no browser-default blue)

## Markdown Rendering

All markdown contexts (task descriptions, message bodies, in-flight messages) use `react-markdown` with `remark-gfm`. The styling is applied via the `.detail-md` class hierarchy:

- Links: `var(--accent-bright)` with underline in `var(--border-bright)`, brightens on hover
- Code: mono font, `var(--gold)` on `var(--bg-3)` with border
- Pre: `var(--bg-0)` background, border, mono 11px
- Blockquote: gold-dim left border, italic, muted
- Dropcap: gothic `::first-letter` on first paragraph, 3.5em, accent color, floated left
- Tables: display font headers, uppercase, bordered cells

Every sub-context (`.mini-task-body`, `.inflight-msg-body`, `.task-expanded-body`) inherits these link and code styles explicitly. No markdown context should ever show browser-default blue links.
