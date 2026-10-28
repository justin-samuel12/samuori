package recipedef

import (
	"errors"
	"strings"
	"testing"
)

const pin = "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func base() string {
	return `
id: mssql-tooling
version: 1.0.0
displayName: SQL Server base with tooling
platforms: [linux/amd64]
egressPresets: [ubuntu]
secrets: [sa_password]
parameters:
  - {name: nodeVersion, type: version, default: "24.1.0"}
  - {name: tag, type: string, default: base}
stages:
  - name: configure
    image: mcr.microsoft.com/mssql/server:2025-latest` + pin + `
    steps:
      - workdir: /opt/build
      - copySource: {from: scripts, to: /opt/build/scripts}
      - run: {shell: "curl -fsSL https://nodejs.org/dist/v$nodeVersion/node.tar.xz -o /tmp/n", network: egress}
      - run: {argv: [/opt/build/scripts/configure.sh, "${tag}"], secrets: [sa_password]}
  - name: final
    from: configure
    steps:
      - copyFromStage: {stage: configure, from: /var/opt/mssql/data, to: /var/opt/mssql/data, owner: "10001"}
      - env: {name: MSSQL_PID, value: Developer}
      - label: {name: org.opencontainers.image.title, value: "${tag}"}
      - expose: "1433"
      - user: mssql
      - cmd: [/opt/mssql/bin/sqlservr]
`
}

func TestCustomRecipeRendering(t *testing.T) {
	t.Run("SEC-SPC-06", func(t *testing.T) {
		d, err := Parse([]byte(base()))
		if err != nil {
			t.Fatal(err)
		}
		params, err := d.Resolve(map[string]string{"tag": "release-1"})
		if err != nil {
			t.Fatal(err)
		}
		out, err := d.Render(params, map[string]Mount{"sa_password": {ID: "sa_password", Target: "/run/secrets/sa_password", Required: true}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		df := string(out)
		for _, want := range []string{
			"FROM mcr.microsoft.com/mssql/server:2025-latest" + pin + " AS configure",
			"ARG nodeVersion\nARG tag\n",
			`RUN ["/bin/sh","-c","curl -fsSL https://nodejs.org/dist/v$nodeVersion/node.tar.xz -o /tmp/n"]`,
			`RUN --network=none --mount=type=secret,id=sa_password,target=/run/secrets/sa_password,required=true ["/opt/build/scripts/configure.sh","release-1"]`,
			`COPY --from=configure --chown=10001 ["/var/opt/mssql/data","/var/opt/mssql/data"]`,
			`LABEL "org.opencontainers.image.title"="release-1"`,
			"USER mssql\nCMD [\"/opt/mssql/bin/sqlservr\"]",
		} {
			if !strings.Contains(df, want) {
				t.Errorf("rendered Dockerfile lacks %q:\n%s", want, df)
			}
		}

		// A parameter whose pattern allows anything still cannot add instructions in any field.
		open := strings.Replace(base(), "{name: tag, type: string, default: base}", `{name: tag, type: string, default: base, pattern: ".*"}`, 1)
		d, err = Parse([]byte(open))
		if err != nil {
			t.Fatal(err)
		}
		for _, evil := range []string{"x\nRUN curl evil", `x"]` + "\n" + `RUN ["evil`, "x; rm -rf /", "$(id)"} {
			if _, err := d.Resolve(map[string]string{"tag": evil}); err == nil {
				t.Errorf("parameter value %q was accepted", evil)
			}
		}
		withEvil := map[string]string{"nodeVersion": "24.1.0", "tag": "x\nRUN curl evil"}
		if out, err := d.Render(withEvil, nil, nil); err == nil && strings.Contains(string(out), "\nRUN curl evil") {
			t.Errorf("a newline in a parameter became an instruction:\n%s", out)
		}

		refused := map[string]string{
			"an unpinned image":         strings.Replace(base(), pin, "", 1),
			"an unknown step (ADD)":     strings.Replace(base(), "- workdir: /opt/build", "- add: {from: http://x, to: /x}", 1),
			"a final root user":         strings.Replace(base(), "- user: mssql", "- user: root", 1),
			"no final user":             strings.Replace(base(), "      - user: mssql\n", "", 1),
			"a secret-looking env":      strings.Replace(base(), "name: MSSQL_PID", "name: SA_PASSWORD", 1),
			"${param} in a shell step":  strings.Replace(base(), "v$nodeVersion", "v${nodeVersion}", 1),
			"an undeclared secret":      strings.Replace(base(), "secrets: [sa_password]}", "secrets: [other]}", 1),
			"a later stage as source":   strings.Replace(base(), "from: configure\n", "from: final\n", 1),
			"a path escaping the image": strings.Replace(base(), "workdir: /opt/build", "workdir: /opt/../etc", 1),
			"an unsupported platform":   strings.Replace(base(), "[linux/amd64]", "[windows/amd64]", 1),
			"an unknown parameter":      strings.Replace(base(), `"${tag}"]`, `"${other}"]`, 1),
			"a plan runtime image":      strings.Replace(base(), "    image: mcr.microsoft.com/mssql/server:2025-latest"+pin, "    runtime: build", 1),
			"the BuildSpec's command":   strings.Replace(base(), "- workdir: /opt/build", "- run: {command: install, network: egress}", 1),
			"the BuildSpec's start":     strings.Replace(base(), "- cmd: [/opt/mssql/bin/sqlservr]", "- start: true", 1),
			"the application port":      strings.Replace(base(), `- expose: "1433"`, "- exposeAppPort: true", 1),
			"an unnamed final stage":    strings.Replace(base(), "  - name: final\n    from:", "  - from:", 1),
		}
		for name, text := range refused {
			var inv *Invalid
			if _, err := Parse([]byte(text)); !errors.As(err, &inv) {
				t.Errorf("%s was accepted: %v", name, err)
			}
		}
		root := strings.Replace(base(), "- user: mssql", "- user: root", 1) + "runsAsRoot: SQL Server setup needs root\n"
		if _, err := Parse([]byte(root)); err != nil {
			t.Errorf("a recorded runsAsRoot reason was not accepted: %v", err)
		}

		d, _ = Parse([]byte(base()))
		forged := d
		forged.Stages = append([]Stage{}, d.Stages...)
		forged.Stages[0].Steps = append([]Step{{Run: &Run{Command: CommandInstall}}}, d.Stages[0].Steps...)
		if out, err := forged.Render(map[string]string{"nodeVersion": "24.1.0", "tag": "x"}, nil, nil); err == nil {
			t.Errorf("a custom recipe from a plan ran the BuildSpec's command:\n%s", out)
		}
		if _, err := d.Resolve(map[string]string{"unknown": "x"}); err == nil {
			t.Error("an unknown parameter value was accepted")
		}
		if _, err := d.Resolve(map[string]string{"nodeVersion": "latest; curl"}); err == nil {
			t.Error("a malformed version was accepted")
		}
	})
}
