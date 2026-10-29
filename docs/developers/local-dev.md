# Local Development Server

`make dev` builds the UI and `samuori-server` and runs them on `http://127.0.0.1:8080`, keeping all state in
`~/.cache/samuori/dev` (override with `DEV_DIR`). The first start creates the organization "Dev" and the administrator
`admin` / `admin1` (set `SAMUORI_DEV_ADMIN_PASSWORD` before the first start to choose another). Later starts reuse the
same data: teams, projects, users and secrets survive restarts and rebuilds.

On this server every local account may use a short password: the convention is the username plus `1`, such as
`admin1` or `dana1`. `make dev` allows that with `--dev-weak-passwords=true`; only an empty password is still
refused. The server refuses the setting unless it is an all-in-one server on a loopback address (SEC-AUTH-10), so
Compose, clusters and every real installation keep the full password policy. Never use it elsewhere. The forms leave
the policy to the server, so the same short passwords work in Setup and Users.

## Screenshots

`make screenshots` builds the UI and `samuori-server`, starts a throwaway server on `127.0.0.1:18090` (override with
`SAMUORI_TOUR_PORT`) with its own data directory under `/tmp`, seeds example data, captures every scene in
`ui/e2e/tour/scenes.ts` in light and dark into `dist/screenshots/` with a gallery page, copies the images the README
shows into `docs/screenshots/`, and deletes the server and its data. It never touches `~/.cache/samuori/dev`. A file
is rewritten only when it changed.

The full gallery lives on the GitHub wiki, so the repository holds only the README's images. `make screenshots-wiki`
clones the wiki into `dist/wiki` (once its first page exists), replaces the gallery and the short Home page, and
commits them with this repository's own Git identity; review it, then publish with `git -C dist/wiki push`. Set
`WIKI_URL` (for example in `local.mk`) when you reach GitHub through a different SSH host name.

Teams, users, projects, uploads, specs, builds, the registry, the pool, secrets and the request go through the public
API. A local server has no worker, so `ui/e2e/tour/seed.ts` writes what only a worker produces (finished, running
and failed builds with their logs, discovery results, validation, the enrolled worker and a publication) straight
into the throwaway SQLite database, and refuses any data directory the script did not create. Secret values are
random and never displayed.

`make screenshots-check` (part of `make ci`) fails when a route in `ui/src/App.tsx` has no scene and no reason in
`notCaptured`. A change that adds or changes a screen updates its scene and runs `make screenshots`.

## Two-node VMs

`deploy/dev/lima/two-node.sh` runs a disposable controller (`samuori-clean`) and remote worker (`samuori-remote`) in Lima:

- `up` builds the release, deletes and recreates both VMs, installs them, and runs the two-node acceptance test to
  create `admin` / `admin1`, `dana` / `dana1` (maintainer of team `payments`), an enrolled worker, the local registry
  and a first build of `orders-api`. The UI is at `http://127.0.0.1:18480`. `up --no-setup` stops before setup.
- `update` builds a signed bundle one patch version ahead with `make update`, uploads it, prints the checks and
  rolls it out through the API; it passes only when the controller and the worker run the new version. Run
  `make release-key` before `up`, so both hosts trust that key.
- `down` deletes both VMs; `clean` also removes `dist/` and Lima's download cache; `status` lists them.

The controller listens on loopback, so `up` enables `dev.weakPasswords` there with a systemd drop-in
(`SAMUORI_DEV_WEAK_PASSWORDS=true`). Never copy that drop-in to another host.

## Cluster VMs

`deploy/dev/lima/cluster.sh` runs three disposable controllers in Lima: `samuori-clean` also hosts PostgreSQL and a zot
registry over TLS from a throwaway CA (blob storage, `--oci-repository`), and `samuori-remote` and `samuori-ctrl3` join it.

- `up` builds the release, recreates the three VMs, runs `init` on `samuori-clean` and joins the other two, accepting each
  request only when the verification code the host printed matches the portal's.
- `test` removes `samuori-ctrl3` and checks its host was wiped, checks that removing one of the two left is refused,
  rejoins `samuori-ctrl3` from a fresh copy of the release, then removes the leader through another controller.
- `update` (after `make release-key` and `up`) rolls a signed bundle out to the three controllers and checks the
  leader went last, then a signed but deliberately broken release that must roll back on the first host and pause;
  `SAMUORI_UPDATE_PATCH=n` picks the version, so it can run again on the same cluster.
- `down` deletes the VMs.

- Stop it with Ctrl-C. `make dev` again picks up code changes.
- `make dev-reset` deletes the data; the next start bootstraps a fresh organization with the same password.
- For hot reload of UI code, keep `make dev` running and run `npm run dev` in `ui/` (port 5173, API proxied to 8080).
- It runs on macOS too, but uploads are not analysed and nothing builds there: discovery and workers need Linux. For
  real builds, keep a Lima VM (`deploy/dev/lima/samuori-clean.yml`) between sessions with `limactl stop` and
  `limactl start` instead of deleting it; `make aio-clean` upgrades it in place, and `deploy/dev/lima/reset-clean.sh`
  returns it to a just-installed state.
