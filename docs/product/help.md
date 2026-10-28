# In-App Help

Owner decision, 2026-10-21. Every signed-in user has **Help** in the account menu (top right) and in the search
palette (Ctrl+K / ⌘K), with task-based guides written for users. A user sees only the guides for the roles they
hold; the server filters, so a guide outside the user's roles is not found, even by its address.

## Guides

- One Markdown file per guide in `internal/help/content/`, embedded in `samuori-server`, so guides always match the
  installed release and need no network.
- Front matter: `title`, `section`, `summary`, `order` and `roles`. `roles` lists who may read it: any of the six
  roles, or `everyone` for every signed-in user, including users without a role. A guide with no roles or an
  unknown role fails to load, so a mistake cannot publish a guide to everyone.
- Roles add up: a user who holds several roles at any scope sees every guide for any of them. Roles do not imply
  one another, so an administrator sees administrator guides, not developer guides.
- Supported Markdown: `##` and `###` headings, paragraphs, lists with one nested level, tables, `**bold**`,
  `` `code` ``, links to other guides (`help:<id>`) and to app pages (`/path`). The UI renders it as React elements,
  never as HTML. A link to a guide the reader cannot open is shown as plain text.
- Guides describe what the UI really does; a feature without UI is not described as if it had one.

## API

`GET /help/topics` lists the caller's guides; `GET /help/topics/{id}` returns one, or 404 when it does not exist or
the caller's roles do not include it. Both are open to any signed-in user (`x-samuori-roles: [any]`). SEC-RBAC-09
covers the filtering, the load checks and broken links between guides.

## In the UI

`PageHeader` takes `help="<id>"`; a **?** next to the page title links to that guide, shown only when the server
lists it for the user.
