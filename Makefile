-include local.mk

GO ?= go
NPM = $(if $(NODE_BIN),PATH="$(NODE_BIN):$$PATH" npm,npm)
export GOTOOLCHAIN := auto
PKGS = $$($(GO) list ./... | grep -v /node_modules/)
GO_FILES = $$(git ls-files -co --exclude-standard '*.go' | grep -v /node_modules/)

.PHONY: dev dev-reset screenshots screenshots-wiki screenshots-check test test-vm test-acceptance test-acceptance-two-node release-key release-artifacts update release-verify license-check sbom vuln vuln-ui release aio-clean aio-compose aio-debian aio-amd64 build generate check-generated fmt fmt-check vet api-check yml-check ui-install ui-check ci

# Every build is a development build of the version in VERSION until a release is published.
VERSION ?= $(shell cat VERSION)-dev+$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS = -X samuori.invalid/samuori/internal/version.Version=$(VERSION)

build:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

RELEASE_ARCH ?= arm64
RELEASE = dist/samuori-linux-$(RELEASE_ARCH)

release:
	rm -rf $(RELEASE) && mkdir -p $(RELEASE)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(RELEASE_ARCH) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(RELEASE)/ ./cmd/samuori-server ./cmd/samuori-worker ./cmd/samuori ./cmd/samuori-web
	cd ui && $(NPM) run -s build
	cp -R ui/dist $(RELEASE)/ui
	cd tools/licensecheck && $(GO) run . -root ../.. -notices ../../$(RELEASE)/ui/licenses/THIRD_PARTY_NOTICES.txt
	cp LICENSE NOTICE $(RELEASE)/
	cp deploy/systemd/* deploy/compose/* $(RELEASE)/

# Signed release artifacts, built and verified locally only; nothing is published
# (docs/developers/release.md). RELEASE_KEY is an SSH signing key; RELEASE_SIGNERS lists the allowed signers.
# Both default to dist/signing, which make release-key creates once and no target deletes.
SIGNING = dist/signing
RELEASE_KEY ?= $(SIGNING)/release
RELEASE_SIGNERS ?= $(SIGNING)/allowed_signers
ARTIFACTS = dist/artifacts

release-key:
	@if [ -f $(SIGNING)/release ]; then echo "$(SIGNING)/release already exists; it is kept"; else \
		mkdir -p $(SIGNING) && chmod 700 $(SIGNING) && \
		ssh-keygen -q -t ed25519 -N '' -C release@samuori.invalid -f $(SIGNING)/release && \
		echo "release@samuori.invalid namespaces=\"samuori-release\" $$(cat $(SIGNING)/release.pub)" >$(SIGNING)/allowed_signers && \
		echo "created $(SIGNING)/release (private; back it up) and $(SIGNING)/allowed_signers (for --release-signers)"; fi

release-artifacts: release sbom
	@[ -f "$(RELEASE_KEY)" ] || { echo "no signing key at $(RELEASE_KEY); run make release-key first (docs/developers/release.md)"; exit 1; }
	rm -rf $(ARTIFACTS) && mkdir -p $(ARTIFACTS)
	COPYFILE_DISABLE=1 tar --no-xattrs -C dist -czf $(ARTIFACTS)/samuori-$(VERSION)-linux-$(RELEASE_ARCH).tar.gz samuori-linux-$(RELEASE_ARCH)
	cp dist/sbom.spdx.json $(ARTIFACTS)/samuori-$(VERSION).spdx.json
	cp dist/THIRD_PARTY_NOTICES.txt $(ARTIFACTS)/
	cd $(ARTIFACTS) && shasum -a 256 *.tar.gz *.spdx.json THIRD_PARTY_NOTICES.txt >SHA256SUMS
	ssh-keygen -Y sign -q -n samuori-release -f $(RELEASE_KEY) $(ARTIFACTS)/SHA256SUMS

# One signed update bundle with every architecture in UPDATE_ARCHES, for upload under Upgrades (ADR 0045).
UPDATE_ARCHES ?= amd64 arm64
UPDATE_DIR = dist/update

# Starts by emptying dist/, keeping the signing directory and whatever holds RELEASE_KEY or RELEASE_SIGNERS.
update:
	@[ -f "$(RELEASE_KEY)" ] || { echo "no signing key at $(RELEASE_KEY); run make release-key first (docs/developers/release.md)"; exit 1; }
	@for p in dist/*; do \
		case "$$p" in $(SIGNING)) continue;; esac; \
		case "$(RELEASE_KEY)/" in "$$p"/*) continue;; esac; \
		case "$(RELEASE_SIGNERS)/" in "$$p"/*) continue;; esac; \
		[ -e "$$p" ] && echo "removing $$p" && rm -rf "$$p"; \
	done; true
	mkdir -p $(UPDATE_DIR)
	git log --no-merges --format='%h%x09%cs%x09%s' > $(UPDATE_DIR)/CHANGES.tsv
	for a in $(UPDATE_ARCHES); do \
		$(MAKE) release RELEASE_ARCH=$$a VERSION=$(VERSION) && \
		cp $(UPDATE_DIR)/CHANGES.tsv dist/samuori-linux-$$a/ && \
		COPYFILE_DISABLE=1 tar --no-xattrs -C dist -czf $(UPDATE_DIR)/samuori-$(VERSION)-linux-$$a.tar.gz samuori-linux-$$a || exit 1; \
	done
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/samuori-server update-bundle -key $(RELEASE_KEY) -out dist/samuori-$(VERSION).samuoriupdate $(UPDATE_DIR)/*.tar.gz

release-verify:
	@[ -f "$(RELEASE_SIGNERS)" ] || { echo "no allowed_signers at $(RELEASE_SIGNERS); run make release-key first (docs/developers/release.md)"; exit 1; }
	cd $(ARTIFACTS) && shasum -a 256 -c SHA256SUMS
	ssh-keygen -Y verify -n samuori-release -f $(RELEASE_SIGNERS) -I release@samuori.invalid -s $(ARTIFACTS)/SHA256SUMS.sig < $(ARTIFACTS)/SHA256SUMS

aio-clean: release
	deploy/dev/lima/install-clean.sh $(RELEASE)

# The browser journey (upload to publication) against the all-in-one install in samuori-clean (make aio-clean).
test-acceptance:
	cd ui && SAMUORI_ACCEPTANCE_URL=http://127.0.0.1:18480 SAMUORI_ACCEPTANCE_SHELL="limactl shell samuori-clean" $(NPM) run -s acceptance

test-acceptance-two-node:
	cd ui && SAMUORI_ACCEPTANCE_URL=http://127.0.0.1:18480 SAMUORI_ACCEPTANCE_SHELL="limactl shell samuori-clean" SAMUORI_ACCEPTANCE_WORKER_SHELL="limactl shell samuori-remote" $(NPM) run -s acceptance -- e2e/acceptance/two-node.spec.ts

aio-compose: release
	deploy/dev/lima/install-clean.sh $(RELEASE) samuori-compose --server-in-compose

aio-debian: release
	deploy/dev/lima/install-clean.sh $(RELEASE) samuori-debian

aio-amd64:
	$(MAKE) release RELEASE_ARCH=amd64
	deploy/dev/lima/install-clean.sh dist/samuori-linux-amd64 samuori-amd64

generate:
	$(GO) tool oapi-codegen -config internal/api/apigen/oapi-codegen.yml api/openapi/samuori.v1.yml
	$(GO) tool oapi-codegen -config internal/apiclient/oapi-codegen.yml api/openapi/samuori.v1.yml
	cd tools/genunimpl && $(GO) run . -in ../../internal/api/apigen/api.gen.go -out ../../internal/api/apigen/unimplemented.gen.go
	cd tools/protogen && $(GO) run .
	$(GO) run ./tools/genroles -spec api/openapi/samuori.v1.yml -out ui/src/api/generated/roles.ts
	cd ui && $(NPM) run -s generate:api

check-generated: generate
	git diff --exit-code -- internal/api/apigen internal/apiclient internal/workerproto ui/src/api/generated
	@untracked="$$(git ls-files -o --exclude-standard internal/api/apigen internal/apiclient internal/workerproto ui/src/api/generated)"; if [ -n "$$untracked" ]; then echo "untracked generated files:"; echo "$$untracked"; exit 1; fi

fmt:
	gofmt -s -w $(GO_FILES)

fmt-check:
	@out="$$(gofmt -s -l $(GO_FILES))"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet $(PKGS)
	cd tools/apicheck && $(GO) vet ./...

# Focused regression tests for security boundaries (ADR 0039, as amended); the seccomp and sandbox ones need Linux.
test:
	$(GO) test -race -count=1 -timeout 30m $(PKGS)
	cd tools/licensecheck && $(GO) test -count=1 ./...

test-vm:
	deploy/dev/lima/sync.sh
	limactl shell samuori-dev sh -c 'cd ~/samuori && PATH=/usr/local/go/bin:$$PATH GOTOOLCHAIN=local go test -race -count=1 -timeout 30m ./...'

# License policy for everything linked or bundled (docs/dependencies.md#license-policy).
license-check:
	cd tools/licensecheck && $(GO) vet ./... && $(GO) run . -root ../..

sbom:
	mkdir -p dist
	cd tools/licensecheck && $(GO) run . -root ../.. -sbom ../../dist/sbom.spdx.json -notices ../../dist/THIRD_PARTY_NOTICES.txt

GOVULNCHECK = golang.org/x/vuln/cmd/govulncheck@v1.8.0
VULNDB = $(HOME)/.cache/samuori/vulndb

# Downloads the whole Go vulnerability database and checks offline, so no dependency list leaves this machine.
vuln:
	rm -rf $(VULNDB) && mkdir -p $(VULNDB)
	curl -fsSL -o $(VULNDB).zip https://vuln.go.dev/vulndb.zip
	cd $(VULNDB) && unzip -q $(VULNDB).zip
	$(GO) run $(GOVULNCHECK) -db file://$(VULNDB) ./...

# Sends ui/package-lock.json to the npm registry's audit service.
vuln-ui:
	cd ui && $(NPM) audit --omit=dev

api-check:
	cd tools/apicheck && $(GO) run . -spec ../../api/openapi/samuori.v1.yml -proto ../../api/proto/samuori/worker/v1/worker.proto

yml-check:
	@bad="$$(git ls-files -co --exclude-standard '*.yaml' | grep -vxE 'deploy/helm/samuori/(Chart|values)\.yaml' || true)"; if [ -n "$$bad" ]; then echo "use .yml (see docs/developers/toolchain.md):"; echo "$$bad"; exit 1; fi



DEV_DIR ?= $(HOME)/.cache/samuori/dev

dev:
	cd ui && $(NPM) run -s build
	$(GO) build -o $(DEV_DIR)/samuori-server ./cmd/samuori-server
	deploy/dev/run-local.sh $(DEV_DIR) $(CURDIR)/ui/dist

dev-reset:
	rm -rf $(DEV_DIR)/data

# The screenshot tour on a throwaway server; the version has no commit, so unchanged screens stay byte-identical.
TOUR_DIR = bin/tour

screenshots:
	cd ui && $(NPM) run -s build
	CGO_ENABLED=0 $(GO) build -ldflags "-X samuori.invalid/samuori/internal/version.Version=$(shell cat VERSION)-dev" -o $(TOUR_DIR)/samuori-server ./cmd/samuori-server
	$(if $(NODE_BIN),PATH="$(NODE_BIN):$$PATH") deploy/dev/screenshots.sh $(TOUR_DIR)/samuori-server $(CURDIR)/ui/dist

# The full gallery goes to the GitHub wiki, keeping the repository to the README's images.
WIKI_URL ?= git@github.com:justin-samuel12/samuori.wiki.git

screenshots-wiki:
	deploy/dev/screenshots-wiki.sh $(WIKI_URL)

screenshots-check:
	cd ui && $(if $(NODE_BIN),PATH="$(NODE_BIN):$$PATH") node e2e/tour/coverage.ts

ui-install:
	cd ui && $(NPM) ci

ui-check:
	cd ui && $(NPM) run -s generate:api && git diff --exit-code -- src/api/generated
	cd ui && $(NPM) run -s typecheck && $(NPM) run -s test && $(NPM) run -s lint && $(NPM) run -s format:check && $(NPM) run -s build

ci: check-generated fmt-check vet build test api-check yml-check ui-check screenshots-check license-check
