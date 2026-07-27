# Table columns and editable row fields as descriptor arrays

> **Status: IMPLEMENTED.** Implemented 2026-07-27: `COLUMNS` and `EDIT_FIELDS` in `src/server/ui/index.html`; the header, row cells, cell refs, repaint pass and sort keys are all generated from `COLUMNS`.

**Pattern** — adding one column to the instances table means editing six places
in `src/server/ui/index.html`; adding one editable row field means eleven. Three
of the column edit sites fail silently when forgotten. The cost has already
stopped work twice.

## Files

All in `src/server/ui/index.html`:

- Columns: `:134-143` (`<th data-sort>`), `:570-588` (row template `<td class="c-*">`),
  `:611-615` (cell refs on the row controller), `:656-661` (render calls in
  `fetchStatuses`), `:732-745` (`sortValue` switch), `:31/:37` (CSS widths).
- Editable fields: `resolveBaseline` `:333-339`, controls `:574-576`,
  `querySelector`+seed `:595-604`, `ctrl` keys `:607-609`, `recomputeChanged`
  `:558-564`, listeners `:632-638`, `setRowBusy` `:779`, query assembly `:798-800`,
  baseline reset `:820-822`.

## Evidence the cost is real

`bidPrice` is fully wired server-side (`handlers.go` reads `q.Get("bidPrice")`,
`/api/config` publishes `defaultBidPrice`, the CLI has `--bid-price`) and appears
**0 times** in `index.html`. `volumeSize` is emitted by `/api/instances` and also
appears **0 times**. Two features stalled at the UI boundary.

## Silent-failure modes today

1. `<th>` without a matching `<td>` — every later cell renders under the wrong
   header. No error.
2. `<th data-sort="k">` without a `sortValue` case — `sortValue` returns `""`,
   `isMissing` makes every row tie, `Array.sort` is stable, so the header paints
   its `sorted-asc` arrow and **nothing reorders**.
3. A missing render call in `fetchStatuses` — the cell keeps its `—`/`…`
   placeholder forever.

## Proposed change

```js
// One entry per instances-table column. `cell` is the <td>'s initial innerHTML;
// `render(ctrl, st)` repaints it from a status snapshot (omit for columns that
// only change on rebuild); `sort(ctrl)` is the sort key (omit => not sortable,
// and no data-sort attribute is emitted).
const COLUMNS = [
  { key: "name",  label: "Instance", init: initNameCell, sort: c => c.name },
  { key: "state", label: "State", cell: `<span class="st st-pending">…</span>`,
    render: renderStatus, sort: c => (c.st && c.st.state) || "" },
  { key: "specs", label: "Specs", title: "vCPU / memory / GPU of the running instance",
    render: renderSpecs, sort: c => c.st?.vCpus ?? NaN },
  // … type, req, az, price, runtime, total, actions
];
```

Three generated sites replace all six:

```js
els.thead.querySelector("tr").innerHTML = COLUMNS.map(c =>
  `<th${c.sort ? ` data-sort="${c.key}"` : ""}${c.title ? ` title="${escapeHtml(c.title)}"` : ""}>${c.label}</th>`).join("");
tr.innerHTML = COLUMNS.map(c => `<td class="c-${c.key}">${c.cell || "—"}</td>`).join("");
ctrl.cells = Object.fromEntries(COLUMNS.map(c => [c.key, tr.querySelector(".c-" + c.key)]));
// fetchStatuses: for (const c of COLUMNS) c.render?.(ctrl, map[name]);
// sortValue:     COLUMNS.find(c => c.key === key)?.sort(ctrl) ?? "";
```

And for the editable fields:

```js
// Row fields the user can override before start/restart: baselined, highlighted
// amber when changed, sent as `param`, re-baselined after a successful run.
const EDIT_FIELDS = [
  { key: "type",    param: "instanceType", el: c => c.typeInput, get: c => c.typeInput.value.trim() },
  { key: "request", param: "requestType",  el: c => c.reqSelect, get: c => c.reqSelect.value },
  { key: "az",      param: "az",           el: c => c.azInput,   get: c => c.azInput.value.trim() },
];
```

`recomputeChanged`, the `runRowAction` query assembly, `setRowBusy` and the
post-run baseline reset each become one loop. Adding `bidPrice` becomes one
entry plus its control.

**Do not** fold the render functions into a uniform slot blindly: `renderRuntime`
is async and writes two cells, and `syncTypeToRunning` must run before the
others. Keep ordering explicit (`init` / `render` / a separate pre-pass).

## LOC estimate

Added ~40 (two descriptor arrays + generated wiring). Removed ~70. Net **−30**.
Per new column: 6 edit sites → 1. Per new editable field: 11 → 2.

## Risk

UI only — no Go, no wire format, no persisted data. The table markup is generated
rather than literal, so verify the rendered header/cell order matches the current
page before/after (`curl / | grep -o 'c-[a-z]*'`). No tests cover this file today.

## Existing precedent

`optionsHTML(values, selected)` (`index.html:260`) is the in-file precedent for
collapsing a repeated markup builder into one helper driven by data.
