package discovery

import (
	"bytes"
	"encoding/xml"
	"strings"
)

type msbuildProject struct {
	Sdk    string `xml:"Sdk,attr"`
	Groups []struct {
		TargetFramework  string `xml:"TargetFramework"`
		TargetFrameworks string `xml:"TargetFrameworks"`
		OutputType       string `xml:"OutputType"`
		AssemblyName     string `xml:"AssemblyName"`
		UseWPF           string `xml:"UseWPF"`
		UseWindowsForms  string `xml:"UseWindowsForms"`
	} `xml:"PropertyGroup"`
	Items []struct {
		Packages []struct {
			Include string `xml:"Include,attr"`
		} `xml:"PackageReference"`
	} `xml:"ItemGroup"`
}

var dotnetProjects = ext(".csproj", ".fsproj", ".vbproj")

func detectDotnet(t *Tree) []Finding {
	var out []Finding
	for _, dir := range t.Dirs(dotnetProjects) {
		for _, name := range t.Names(dir, dotnetProjects) {
			out = append(out, dotnetFinding(t, dir, name))
		}
	}
	if slns := t.Names(".", ext(".sln", ".slnx")); len(slns) > 0 && len(out) > 1 {
		for i := range out {
			out[i].Warnings = append(out[i].Warnings, Warning{"solution-member", "The solution " + slns[0] + " has several projects; choose the one to build.", slns[0]})
		}
	}
	return out
}

func dotnetFinding(t *Tree, dir, name string) Finding {
	f := newFinding("dotnet", "1.0.0", dir)
	p := join(dir, name)
	f.Language = map[string]string{".csproj": "csharp", ".fsproj": "fsharp", ".vbproj": "visualbasic"}[strings.ToLower(name[strings.LastIndex(name, "."):])]
	f.PackageManager = "nuget"
	raw, ok := t.Read(p)
	var proj msbuildProject
	if !ok || xml.NewDecoder(bytes.NewReader(raw)).Decode(&proj) != nil {
		f.add(Weak, "manifest", p, "", "project file exists but could not be read")
		f.warn("manifest-unreadable", "The project file is not valid XML or is too large to inspect.", p)
		return f.done()
	}
	var tfm, outputType, assembly string
	windows := false
	for _, g := range proj.Groups {
		tfm = first(tfm, g.TargetFramework, strings.Split(g.TargetFrameworks, ";")[0])
		outputType = first(outputType, g.OutputType)
		assembly = first(assembly, g.AssemblyName)
		windows = windows || strings.EqualFold(g.UseWPF, "true") || strings.EqualFold(g.UseWindowsForms, "true")
	}
	if tfm != "" {
		f.RuntimeConstraints["dotnet"] = tfm
		f.add(Strong, "manifest", p, "TargetFramework", "targets "+tfm)
		if strings.Contains(tfm, "-windows") || (strings.HasPrefix(tfm, "net4") && !strings.Contains(tfm, ".")) || strings.HasPrefix(tfm, "net3") {
			windows = true
		}
	}
	if windows {
		f.RequiredPlatforms = []string{"windows"}
		f.warn("windows-target", "This project targets Windows (.NET Framework, WPF, Windows Forms or a -windows framework). Windows builds are not available yet.", p)
	}
	test := false
	for _, ig := range proj.Items {
		for _, pk := range ig.Packages {
			if pk.Include == "Microsoft.NET.Test.Sdk" {
				test = true
			}
		}
	}
	if t.Has(join(dir, "packages.lock.json")) {
		f.add(Strong, "lockfile", join(dir, "packages.lock.json"), "", "NuGet lock file pins packages")
	}
	if t.Has("global.json") {
		f.add(Moderate, "version-file", "global.json", "", "global.json pins the SDK")
	}
	kinds := map[string]bool{}
	switch {
	case test:
		f.warn("test-project", "This is a test project, not an application.", p)
	case strings.EqualFold(proj.Sdk, "Microsoft.NET.Sdk.Web"):
		kinds["http-service"] = true
		f.Framework = "ASP.NET Core"
		f.add(Moderate, "manifest", p, "Sdk", "uses the web SDK")
	case strings.EqualFold(proj.Sdk, "Microsoft.NET.Sdk.Worker"):
		kinds["worker"] = true
		f.add(Moderate, "manifest", p, "Sdk", "uses the worker SDK")
	case strings.EqualFold(outputType, "Exe") || strings.EqualFold(outputType, "WinExe"):
		kinds["cli-task"] = true
		f.add(Moderate, "manifest", p, "OutputType", "builds an executable")
	default:
		f.warn("library", "This project builds a library; choose the application project that uses it.", p)
	}
	if !test {
		f.ApplicationKind = pickKind(f, kinds, p)
	}
	if assembly == "" {
		assembly = strings.TrimSuffix(name, name[strings.LastIndex(name, "."):])
	}
	f.SuggestedCommands["install"] = "dotnet restore " + name
	f.SuggestedCommands["build"] = "dotnet publish " + name + " -c Release -o out --no-restore"
	if f.ApplicationKind != "" {
		f.SuggestedCommands["start"] = "dotnet out/" + assembly + ".dll"
	}
	if test {
		f.SuggestedCommands["test"] = "dotnet test " + name
	}
	return f.done()
}

func first(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
