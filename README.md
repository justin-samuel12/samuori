<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/samuori-3d-logo-dark.svg">
    <img src="docs/brand/samuori-3d-logo-light.svg" alt="Samuori" width="460">
  </picture>
</p>

<p align="center"><sub><i>Woven into one</i></sub></p>

<p align="center">Created, architected and maintained by <b>Justin Samuel</b>.</p>

---

## Why I built Samuori

There are great open-source tools for building container images. BuildKit is fast and careful. Buildah builds
without a daemon. Cloud Native Buildpacks and Railpack can turn plain source code into an image without anyone
writing a Dockerfile. Syft tells you what is inside an image, Grype and Trivy look for known vulnerabilities, and
Cosign signs the result so other people can trust it.

Each of them is very good at its own job. The hard part starts when you need them all at once. Every team ends up
gluing them together by hand: learning each tool's flags, writing pipeline scripts, and hoping a security check
didn't quietly drop out along the way. Meanwhile the developers who simply want their app in a container usually
don't care about any of it. They know Python, Node or .NET. They shouldn't have to become container experts too.

So I built one place where it all comes together. You upload your project in the browser. Samuori works out how to
build it, builds it in isolation, checks it, signs it and hands you an image you can publish. The tools underneath are
the same great tools. Samuori is the loom: it weaves them into one piece of cloth, adds a friendly face and sensible
defaults, and keeps a record of every step.

The name comes from the same idea. "Sam" is from my name, Samuel, and *ori* (織り) is Japanese for weaving.

## What it does

- **Figures out your project.** It reads the source to find the language, framework and start command, and never runs
  your code while doing so.
- **Builds it your way.** Use your own Dockerfile, a built-in recipe, Cloud Native Buildpacks or Railpack. Builds run
  on BuildKit or Buildah.
- **Keeps builds contained.** Uploaded code builds in rootless isolation, one build per build daemon, with network
  access only where an administrator allows it.
- **Checks every image.** It makes a software bill of materials, scans for vulnerabilities, looks for leaked secrets
  and applies your organization's policies. A skipped check is never shown as a pass.
- **Publishes when you're ready.** It signs images in Cosign's format and pushes them to your registry, or lets you
  download an image for `docker load`.
- **Stays accountable.** Roles, approvals and an audit trail cover every action.
- **Goes a step further if you want.** It can draft Kubernetes manifests with Move2Kube or Kompose.

## Built on open source

Samuori stands on the shoulders of these projects and many smaller libraries. It runs them as separate programs or
uses them as libraries, each under its own license.

| Project | What Samuori uses it for |
|---|---|
| [BuildKit](https://github.com/moby/buildkit) and [Buildah](https://github.com/containers/buildah) | Building images |
| [Cloud Native Buildpacks](https://buildpacks.io) and [Railpack](https://github.com/railwayapp/railpack) | Building from source without a Dockerfile |
| [RootlessKit](https://github.com/rootless-containers/rootlesskit) and [runc](https://github.com/opencontainers/runc) | Rootless build isolation |
| [Syft](https://github.com/anchore/syft), [Grype](https://github.com/anchore/grype) and [Trivy](https://github.com/aquasecurity/trivy) | Software bills of materials and vulnerability scans |
| [Cosign](https://github.com/sigstore/cosign) format | Image signatures |
| [ORAS](https://oras.land) and [go-containerregistry](https://github.com/google/go-containerregistry) | Talking to OCI registries |
| [NATS](https://nats.io) | Messaging between servers and build workers |
| [SQLite](https://sqlite.org) and [PostgreSQL](https://www.postgresql.org) | The database |
| [OpenBao](https://openbao.org) | Secrets and signing keys (optional) |
| [Move2Kube](https://move2kube.konveyor.io) and [Kompose](https://kompose.io) | Kubernetes manifests (optional) |
| [React](https://react.dev) and [React Aria](https://react-spectrum.adobe.com/react-aria/) | The web interface |

Samuori is an independent project. It is not affiliated with, or endorsed by, any of these projects or their
maintainers. Their licenses are listed in `docs/dependencies.md`, and every release includes
`THIRD_PARTY_NOTICES.txt`.

## No paid services

Everything Samuori needs is open source. It runs on one Linux machine, with no subscription, license key or cloud
account. If you already use AWS, optional connectors can use ECR, Secrets Manager, S3 and KMS, but nothing requires
them.

## Status

Samuori is pre-release. I use it on a real installation, but it is not ready for production yet. The strongest
isolation profile (`vm-per-job`) has not been validated, so treat it as a project to try and give feedback on.
`docs/roadmap.md` shows what exists and what's next.

## Quick start (one Linux VM)

```
make release                                  # builds dist/samuori-linux-arm64 (RELEASE_ARCH=amd64 for x86-64)
sudo dist/samuori-linux-arm64/install.sh --release dist/samuori-linux-arm64 --confirm-dedicated-vm
```

Then follow `docs/operators/all-in-one.md`.

## Documentation

- `docs/README.md`: the map of all documentation
- `docs/operators/`: installing and running Samuori (`runbook.md`)
- `docs/developers/`: toolchain, Git workflow and releases
- `CONTRIBUTING.md` and `SECURITY.md`

## License

Copyright 2026 Justin Samuel. Licensed under the [Apache License, Version 2.0](LICENSE); see [NOTICE](NOTICE).

The Samuori name and logo identify this project. The logo files are in `docs/brand/`. The wordmark's letters are
outlined from the DejaVu Sans Bold font, whose notice is in `docs/brand/DejaVu-font-notice.txt`.
