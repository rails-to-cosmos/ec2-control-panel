# One definition of the visibility modes

**Pattern** — three visibility states are expressed four different ways, the two
dialogs don't share a vocabulary, and the `"*"` sentinel is a bare literal in the
UI while Go names it.

## Files

- `src/config/instances.go:43` (`ReadersPublic`), `:48-56` (`CanRead`)
- `src/server/ui/index.html:185-187` — Edit dialog radios `admins|users|all`
- `:207-209` — New dialog radios `me|users|all`
- decode `:944`; encode `:956-958` (edit); encode `:984-986` (new);
  `selectedVisibility` `:975-978`
- duplicated `hidden`-toggle listeners `:1008-1012` and `:1016`
- `README.md:121-125` (the mode table)

## Latent inconsistency (verified)

The two dialogs resolve an **unchecked** fieldset to opposite ends of the ACL:

| dialog | fallback | resulting readers |
|---|---|---|
| Edit (`saveEdit`) | `"all"` | `["*"]` → every signed-in user |
| New (`submitNewInstance` → `selectedVisibility`) | `"me"` | `[whoami.user]` → only me |

Not reachable today — both dialogs pre-check a radio before opening — so this is
latent, not an active bug. It is exactly the kind of divergence a single encoder
prevents, and it sits on the closed-by-default ACL that CLAUDE.md calls out.

The vocabularies also differ (`"admins"` vs `"me"`), so neither encoder can be
reused by the other, and the New dialog has no decoder at all — an existing
instance can't round-trip through it.

## Proposed change

Minimal version, JS only:

```js
const READERS_PUBLIC = "*";                     // mirrors config.ReadersPublic

// visibility radio value → readers list. "" = admins only.
const VISIBILITY = {
  admins: ()    => [],
  me:     ()    => (whoami.user ? [whoami.user] : []),
  users:  (sel) => selectedUsers(sel),
  all:    ()    => [READERS_PUBLIC],
};

const visibilityOf = (readers) =>
  readers.includes(READERS_PUBLIC) ? "all" : (readers.length ? "users" : "admins");

// Wires a dialog's radio group + user multi-select; shows the list only for
// "users". Returns { set(readers), readers() }.
function setupVisibilityFieldset(dialog, radioName, usersSel, usersWrap, fallback)
```

Both dialogs then share one encoder, one decoder, one listener and one explicit
fallback. Adding a mode is one `VISIBILITY` entry plus one radio per dialog.

Fuller version — serve the modes from Go so the vocabulary has exactly one home:

```go
// src/config
type VisibilityMode struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	NeedsList bool   `json:"needsList"`
}

var VisibilityModes = []VisibilityMode{ /* admins, me, users, all */ }

func ReadersFor(modeID, self string, picked []string) []string
func ModeOf(readers []string) string
```

Publish on `/api/config`; one JS `visibilityFieldset(prefix, modes)` renders both
dialogs. This also lets `ModeOf` be unit-tested against `CanRead`, which nothing
currently is.

## LOC estimate

JS-only version: added ~26, removed ~28 → net **−2**; the value is the single
closed set and single fallback, not the lines. Merging the two `<dialog>`s into
one `openInstanceDialog({title, name, readers, onSubmit})` takes it to roughly
**−44** and collapses ~12 `els` keys to ~6.

## Risk

JS-only version: UI only, no wire change. The Go version adds a `/api/config`
field (additive, safe) and moves the readers-encoding rule server-side, which
touches the ACL — worth a test asserting `ReadersFor`/`ModeOf` round-trip and
agree with `CanRead`.

## Existing precedent

`src/config/instances.go:43` names the sentinel and `src/server/handlers.go:251`
consumes the constant rather than `"*"` — the UI is the only place the raw literal
survives. `optionsHTML` (`index.html:260`) is the in-file precedent for one
builder serving every picker.
