package discovery

import (
	"regexp"
	"sort"
	"strings"
)

var (
	psRequiresVersion = regexp.MustCompile(`(?im)^\s*#requires\s+-version\s+([0-9.]+)`)
	psRequiresEdition = regexp.MustCompile(`(?im)^\s*#requires\s+-pseditions?\s+(core|desktop)`)
	psRequiresModules = regexp.MustCompile(`(?im)^\s*#requires\s+-modules?\s+(.+)$`)
	psImport          = regexp.MustCompile(`(?im)^\s*(?:import-module|using\s+module)\s+['"]?([A-Za-z0-9_.\-]+)`)
	psManifestVersion = regexp.MustCompile(`(?im)^\s*PowerShellVersion\s*=\s*['"]([0-9.]+)['"]`)
	psManifestEdition = regexp.MustCompile(`(?im)^\s*CompatiblePSEditions\s*=\s*@?\(?([^)\n]+)`)
	psParam           = regexp.MustCompile(`(?im)^\s*param\s*\(`)
	psHTTP            = regexp.MustCompile(`(?i)\b(Start-PodeServer|Start-Polaris|New-PolarisRoute|System\.Net\.HttpListener)\b`)
)

// Windows-only constructs; static analysis cannot prove every runtime requirement.
var psWindowsOnly = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`(?i)\b(HKLM|HKCU|HKCR|HKU|HKCC):|Registry::`), "the Windows registry"},
	{regexp.MustCompile(`(?i)\b(Get|Set|Remove|Invoke)-WmiObject\b|\bRegister-WmiEvent\b`), "WMI cmdlets"},
	{regexp.MustCompile(`(?i)\b(Install|Uninstall|Add|Get)-WindowsFeature\b|\b(Enable|Disable)-WindowsOptionalFeature\b`), "Windows feature management"},
	{regexp.MustCompile(`(?i)\b(Get|Start|Stop|Restart|Set|New)-Service\b`), "Windows service cmdlets"},
	{regexp.MustCompile(`(?i)\b(Get|New|Set|Remove)-LocalUser\b|\b(Get|New|Set)-AD(User|Group|Computer)\b`), "Windows account or Active Directory cmdlets"},
	{regexp.MustCompile(`(?i)\b(Get|Set)-ExecutionPolicy\b`), "execution policy"},
	{regexp.MustCompile(`(?i)\b(msiexec|reg|sc|wmic|robocopy|icacls)(\.exe)?\s`), "Windows executables"},
	{regexp.MustCompile(`(?i)\b[A-Z]:\\[A-Za-z0-9]`), "Windows drive paths"},
}

var psWindowsModules = map[string]bool{"activedirectory": true, "servermanager": true, "dism": true, "webadministration": true, "iisadministration": true, "grouppolicy": true, "hyper-v": true}

func detectPowerShell(t *Tree) []Finding {
	var out []Finding
	for _, dir := range t.Dirs(ext(".ps1", ".psm1", ".psd1")) {
		out = append(out, powershellFinding(t, dir))
	}
	return out
}

func powershellFinding(t *Tree, dir string) Finding {
	f := newFinding("powershell", "1.0.0", dir)
	f.Language = "powershell"
	scripts := t.Names(dir, ext(".ps1"))
	modules := t.Names(dir, ext(".psm1"))
	manifests := t.Names(dir, ext(".psd1"))
	for _, n := range append(append([]string{}, scripts...), modules...) {
		f.add(Weak, "extension", join(dir, n), "", "PowerShell source file")
	}

	windows, desktop := map[string]bool{}, false
	imports := map[string]bool{}
	var httpHint, paramHint bool
	for _, n := range manifests {
		p := join(dir, n)
		raw, ok := t.Read(p)
		if !ok {
			continue
		}
		f.add(Moderate, "manifest", p, "", "module manifest")
		if m := psManifestVersion.FindSubmatch(raw); m != nil {
			f.RuntimeConstraints["powershell"] = ">=" + string(m[1])
			f.add(Moderate, "manifest", p, "PowerShellVersion", "requires PowerShell "+string(m[1]))
		}
		if m := psManifestEdition.FindSubmatch(raw); m != nil {
			editions := strings.ToLower(string(m[1]))
			if strings.Contains(editions, "desktop") && !strings.Contains(editions, "core") {
				desktop = true
				f.add(Strong, "manifest", p, "CompatiblePSEditions", "supports Windows PowerShell (Desktop) only")
			}
		}
	}
	for _, n := range append(append([]string{}, scripts...), modules...) {
		p := join(dir, n)
		raw, ok := t.Read(p)
		if !ok {
			continue
		}
		if m := psRequiresVersion.FindSubmatch(raw); m != nil {
			f.RuntimeConstraints["powershell"] = ">=" + string(m[1])
			f.add(Moderate, "directive", p, "#requires -Version", "requires PowerShell "+string(m[1]))
		}
		if m := psRequiresEdition.FindSubmatch(raw); m != nil {
			if strings.EqualFold(string(m[1]), "desktop") {
				desktop = true
				f.add(Strong, "directive", p, "#requires -PSEdition", "requires Windows PowerShell (Desktop)")
			} else {
				f.add(Moderate, "directive", p, "#requires -PSEdition", "requires PowerShell 7+ (Core)")
			}
		}
		for _, m := range psRequiresModules.FindAllSubmatch(raw, -1) {
			for _, mod := range strings.Split(string(m[1]), ",") {
				if mod = strings.Trim(strings.TrimSpace(mod), `'"@{}`); mod != "" && len(mod) < 100 {
					imports[mod] = true
				}
			}
		}
		for _, m := range psImport.FindAllSubmatch(raw, -1) {
			imports[string(m[1])] = true
		}
		for _, w := range psWindowsOnly {
			if w.re.Match(raw) {
				windows[w.what] = true
				f.warn("windows-specific", "Uses "+w.what+", which is available only on Windows.", p)
			}
		}
		httpHint = httpHint || psHTTP.Match(raw)
		paramHint = paramHint || psParam.Match(raw)
	}
	modNames := make([]string, 0, len(imports))
	for m := range imports {
		modNames = append(modNames, m)
		if psWindowsModules[strings.ToLower(m)] {
			windows["the "+m+" module"] = true
			f.warn("windows-specific", "Imports the "+m+" module, which is available only on Windows.", dir)
		}
	}
	sort.Strings(modNames)
	if len(modNames) > 0 {
		f.add(Weak, "import", dir, "", "imports modules: "+strings.Join(modNames, ", "))
		f.RuntimeConstraints["modules"] = strings.Join(modNames, ",")
	}

	if desktop || len(windows) > 0 {
		f.RequiredPlatforms = []string{"windows"}
		f.warn("windows-target", "This code appears to need Windows. Windows builds are not available yet; Linux builds may fail at run time.", dir)
	} else {
		f.warn("platform-unproven", "No Windows-only commands were found, but static analysis cannot prove the scripts run on Linux.", dir)
	}

	kinds := map[string]bool{}
	if httpHint {
		kinds["http-service"] = true
		f.add(Moderate, "source", dir, "", "starts an HTTP listener")
	} else if len(scripts) > 0 {
		kinds["cli-task"] = true
		if paramHint {
			f.add(Weak, "source", dir, "", "script accepts parameters")
		}
	}
	f.ApplicationKind = pickKind(f, kinds, dir)
	switch len(scripts) {
	case 0:
		if len(modules) > 0 {
			f.warn("module-only", "Only modules were found; choose the script that runs them.", dir)
		}
	case 1:
		f.SuggestedCommands["start"] = "pwsh -NoProfile -File " + scripts[0]
	default:
		f.warn("start-ambiguous", "Several scripts could be the entry point ("+strings.Join(scripts, ", ")+"); choose one.", dir)
	}
	return f.done()
}
