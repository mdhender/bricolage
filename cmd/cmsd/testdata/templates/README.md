# The end-to-end template tree

This is the template tree `cmd/cmsd`'s M8 test points `cmsd --templates` at.

It mirrors the shape `internal/render` searches
(`DESIGN.md` §8.4): a site directory named by the site's domain, the category
tree below it, and one file per element type, named `<key>.gohtml`.

```
htmx-app.localhost/story.gohtml           the site's fallback
htmx-app.localhost/features/story.gohtml  what a story in /features/ gets
htmx-app.localhost/broken/story.gohtml    will not parse; validate mode reports it
htmx-app.localhost/explode/story.gohtml   parses, fails while executing
```

It is a checked-in fixture rather than a tree a helper builds, because nothing
in this repository creates a directory — test helpers included (invariant 19).
Git creates these directories; no Go code in this repository creates any.
