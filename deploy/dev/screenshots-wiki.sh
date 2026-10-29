#!/bin/sh
# Copies the screenshot gallery from dist/screenshots into a local clone of the GitHub wiki and commits it with this
# repository's own identity; pushing stays a manual step (docs/developers/local-dev.md#screenshots).
# Usage: screenshots-wiki.sh <wiki git URL>
set -eu
url=$1
root=$(cd "$(dirname "$0")/../.." && pwd)
src="$root/dist/screenshots"
wiki="$root/dist/wiki"
[ -f "$src/Screenshots.md" ] || { echo "run make screenshots first" >&2; exit 1; }
name=$(git -C "$root" config --local user.name || true)
email=$(git -C "$root" config --local user.email || true)
[ -n "$name" ] && [ -n "$email" ] || { echo "set user.name and user.email in this repository's .git/config first" >&2; exit 1; }
if [ ! -d "$wiki/.git" ]; then
	git clone -q "$url" "$wiki" || {
		echo "the wiki has no pages yet: open the repository's Wiki tab, create the first page, then run this again" >&2
		exit 1
	}
fi
git -C "$wiki" config user.name "$name"
git -C "$wiki" config user.email "$email"
git -C "$wiki" pull -q --ff-only
mkdir -p "$wiki/images"
rm -f "$wiki"/images/*.png
cp "$src"/*.png "$wiki/images/"
cp "$src/Screenshots.md" "$wiki/Screenshots.md"
cat >"$wiki/Home.md" <<'EOF'
# Samuori

A self-hosted container image factory that brings established open-source build, scan and signing tools together
behind one guided, governed workflow.

- [Screenshots](Screenshots): every screen in light and dark, for developers and administrators, with example data.
- [Documentation](https://github.com/justin-samuel12/samuori/tree/main/docs): the authoritative docs live in the
  repository, next to the code they describe.
- [README](https://github.com/justin-samuel12/samuori#readme): what Samuori is, why it exists and how to start.

This wiki holds only the screenshot gallery; `make screenshots-wiki` keeps it and this page up to date.
EOF
git -C "$wiki" add -A
if git -C "$wiki" diff --cached --quiet; then
	echo "the wiki gallery is already up to date"
	exit 0
fi
git -C "$wiki" commit -q -s -m "Update screenshots"
echo "Committed as $name <$email> in $wiki. Review it, then publish with: git -C $wiki push"
