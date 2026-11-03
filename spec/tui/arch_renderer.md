# Renderer — frames, roles, the terminal bracket

## What it is

Our own renderer over `x/term`, chosen over a TUI framework because the
screens are fixed-column text with no widgets: a frame, a role table, a
terminal bracket, and a key decoder, about the size of the report code.

## The frame

A frame is `h` rows of at most `w` cells. A screen renders by filling rows
(`put`) — any row wider than the terminal is cut by visible width with its
colour codes kept balanced (a reset is appended whenever a code was
emitted). Output is one write: cursor home, every row padded to the width
and cleared to the end of line, rows joined with CR LF. The plain form
(codes stripped, trailing spaces trimmed) is what the golden tests pin.
Text is plain ASCII, one cell per character, so widths are counts.

## The role table

Colour is additive and carries no meaning alone: the plain frame reads the
same. Roles map to the 16 palette slots and nothing else — never 256-colour,
never a background — so the terminal theme owns every colour and a themed
terminal (Tokyo Night, Solarized, …) shows its own hues:

| role | slot | where |
|---|---|---|
| chrome | bold magenta | pane titles, footer key letters |
| identifiers | blue | run ids, step names, breadcrumbs |
| status | green `ok`; bold red `FAIL`/`HALT`/`FAILED`; cyan `running`/`LIVE`; yellow `ready`/`WAIT`/`PAUSE`/`slow`; bright black `not needed`/`not reached`/`absent` | the status column, badges |
| tertiary | bright black | rules, hints, column headers, not-part-of-the-story |
| selection | reverse video | the selected row |

Values (durations, params, paths, output text) keep the default foreground.
Colour is on only when the CLI's probe allowed it (a terminal that is not
`TERM=dumb`, `NO_COLOR` unset, or `--color always`).

## The terminal bracket

On entry: raw mode on stdin (when it is a terminal), the alternate screen,
the cursor hidden. On every exit path: cursor shown, main screen, raw mode
restored — the deferred close runs before the quit command is printed, so
the command lands on the operator's normal screen. The size is read from
stdout at every frame, so a resize takes effect on the next redraw; off a
real file (tests) the bracket is a no-op with a fixed size.

## The key decoder

Bytes from stdin become keys: a rune, or a name for the sequences a
terminal sends — CSI `A`/`B`/`C`/`D` arrows, `5~`/`6~` page keys, CR/LF as
enter, a lone ESC as escape. Nothing else is interpreted; mouse reporting
is never enabled.

Requirements implemented: Theme-inheriting renderer.
