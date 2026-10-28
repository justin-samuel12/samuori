# 0055. A frontend and a backend in one image

Status: **Accepted** (2026-10-27, owner decision; provisional, may change during acceptance testing). Extends
[ADR 0005](0005-editor-modes-guided-detailed-yaml.md) and the built-in recipes ([ADR 0041](0041-custom-recipes.md)).

## Context
Many applications keep a browser frontend (Vite, React and similar) and an API backend (Python, Node.js and others)
in one repository and ship them as one image, as the owner's audit application does with nginx and uvicorn under
supervisord. With a Dockerfile, Samuori already builds that. Without one, the guided wizard and the recipes build exactly
one application, so such a project could not get a single image.

## Options
- **Samuori's own web launcher (chosen).** Samuori ships a small static program, `samuori-web`, that serves the built frontend,
  forwards API paths to the backend on localhost, starts the backend and stops when it stops.
- **nginx and supervisord (rejected).** Two more third-party components with generated configuration in every image,
  and the Debian archive at build time.
- **The backend serves the files (rejected).** Works only for applications written to do so, and fails quietly for
  the rest.

## Decision
- **Launcher.** `samuori-web` is original Go code in this repository (`cmd/samuori-web`), built as a static binary for each
  release architecture and shipped with the worker. It listens on one port (8080 by default) as the image's user,
  serves files from the frontend's output folder with a single-page-application fallback to `index.html`, never lists
  directories or serves outside that folder, forwards configured path prefixes (default `/api`) to the backend on
  `127.0.0.1`, starts the backend command as a child, forwards stop signals to it, and exits with the backend's status
  when the backend exits, so the container's restart policy applies.
- **BuildSpec.** A built-in recipe build may add `spec.build.recipe.frontend`: the folder, install and build commands,
  the output folder and the Node.js version, plus the API path prefixes and the port the application listens on behind
  the launcher (`backendPort`, default 8000). `spec.build.recipe.path` names the application's folder (the upload's
  root when omitted). The image listens on `spec.application.port`, which must be an `http-service` port different
  from `backendPort`. The recipe renders a frontend stage, the application's stages, and copies the frontend output
  and the launcher into the final image, whose command becomes the launcher followed by the application's start
  command. Custom recipes refuse both fields.
- **Supply chain.** The frontend's Node.js image comes from the built-in Node.js recipe's runtimes, pinned by digest,
  so organization runtimes and the base-image policy apply; its egress presets join the build's. The frontend's build
  command runs without network, like a recipe's. The worker copies its own `samuori-web` into the build through the local
  named context `samuoriweb` and writes the launcher's SHA-256 digest into the build log. The plan records the launcher
  contract version (`1`); workers that ship the launcher announce it as the provider `samuori-web`, and only those
  workers receive such plans. The launcher is built for the worker's architecture, so a frontend build targets one
  platform.
- **Discovery and wizard.** Discovery already reports one finding per folder. When it finds a Node.js static site and
  an HTTP service in separate folders, the wizard offers "Frontend + backend in one image" and asks for the two
  folders, the frontend's commands and output folder, the API paths and the backend port.

## Consequences
- One process tree, one port and no proxy configuration to maintain; the launcher is part of Samuori's own release and is
  scanned with the image.
- The launcher is deliberately small: no TLS, no caching rules beyond what files need, no load balancing. Projects
  needing more keep using their own Dockerfile.
- Workers and controllers must both support it; plans with a frontend go only to workers that announce the launcher.
- Choosing an application in a subfolder now sets `spec.build.recipe.path`, so a recipe build copies only that
  folder instead of the whole upload.
