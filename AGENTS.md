# Project guidance

## Versioning

Bump `VERSION` without being asked when committing a user-facing change (a
feature or a fix a user would notice) to main: after the change, raise it to
the next release number in its own commit, "Release vX.Y.0: <what changed>".
Bump once per release, not per commit: while `VERSION` is ahead of the newest
`v*` tag, later changes ship under that number. Build-time version strings
and the app bundle versions come from `VERSION` and the tag, so nothing else
is edited. Internal work (tests, docs, refactors), local installs and test
builds keep the current version; never invent preview versions.

Release numbers after v1.1.0 raise the middle number by one, with the last
number at 0: v1.1.0, then v1.11.0, v1.12.0, v1.13.0 and so on. Do not use
v1.2.0 or patch releases such as v1.1.1. Confirm the number with the user
right before tagging.

## Runtime and reference material

Keep active Claude Desktop and CLI sessions intact during development. Use
temporary profiles, mock credentials and isolated upstreams for verification.
Restart or reconfigure the user's running applications only when requested.

Before changing Desktop routing, native credential ownership, or setup, read
`docs/README.md` and the relevant linked contract. The pinned reference projects
and local clone paths are documented in `docs/desktop-reference-map.md`.
