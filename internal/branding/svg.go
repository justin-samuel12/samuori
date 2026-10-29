package branding

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnsafeSVG reports markup that the sanitizer could not parse or reduce to the safe subset.
var ErrUnsafeSVG = errors.New("the SVG could not be sanitized")

// Allowlisted elements and attributes for a static logo. Anything outside these sets is dropped, so a new attack
// surface is excluded by default rather than chased with a denylist. See docs/architecture/security.md#operator-logos.
var allowedElements = map[string]bool{
	"svg": true, "g": true, "path": true, "rect": true, "circle": true, "ellipse": true,
	"line": true, "polyline": true, "polygon": true, "text": true, "tspan": true,
	"defs": true, "lineargradient": true, "radialgradient": true, "stop": true,
	"clippath": true, "mask": true, "title": true, "desc": true, "symbol": true,
	"use": true, "pattern": true, "marker": true,
}

var allowedAttrs = map[string]bool{
	"d": true, "x": true, "y": true, "x1": true, "y1": true, "x2": true, "y2": true,
	"cx": true, "cy": true, "r": true, "rx": true, "ry": true, "dx": true, "dy": true,
	"width": true, "height": true, "points": true, "viewbox": true, "transform": true,
	"fill": true, "stroke": true, "stroke-width": true, "stroke-linecap": true,
	"stroke-linejoin": true, "stroke-dasharray": true, "stroke-dashoffset": true,
	"stroke-miterlimit": true, "opacity": true, "fill-opacity": true, "stroke-opacity": true,
	"fill-rule": true, "clip-rule": true, "clip-path": true, "mask": true, "offset": true,
	"stop-color": true, "stop-opacity": true, "gradientunits": true, "gradienttransform": true,
	"spreadmethod": true, "patternunits": true, "patterntransform": true, "markerwidth": true,
	"markerheight": true, "refx": true, "refy": true, "orient": true, "id": true, "class": true,
	"version": true, "preserveaspectratio": true, "color": true, "display": true,
	"text-anchor": true, "font-family": true, "font-size": true, "font-weight": true,
	"letter-spacing": true, "xml:space": true,
}

// SanitizeSVG parses untrusted SVG and re-serializes only the allowlisted elements and attributes. It drops scripts,
// event handlers, external and non-local references, foreignObject, animation, DOCTYPE/entities, comments and
// processing instructions, so the stored bytes cannot execute or phone out. It also returns the intrinsic width and
// height read from the root element. See docs/architecture/security.md#operator-logos.
func SanitizeSVG(in []byte) (out []byte, width, height float64, err error) {
	dec := xml.NewDecoder(bytes.NewReader(in))
	dec.Strict = false
	dec.Entity = map[string]string{} // no entity expansion; undefined entities become empty

	var buf bytes.Buffer
	skipDepth := 0
	rootSeen := false
	for {
		tok, terr := dec.RawToken()
		if terr != nil {
			if terr.Error() == "EOF" {
				break
			}
			return nil, 0, 0, fmt.Errorf("%w: %v", ErrUnsafeSVG, terr)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			lname := strings.ToLower(local(t.Name))
			if skipDepth > 0 || !allowedElements[lname] {
				skipDepth++
				continue
			}
			if !rootSeen {
				if lname != "svg" {
					return nil, 0, 0, fmt.Errorf("%w: root element is %q, not svg", ErrUnsafeSVG, lname)
				}
				rootSeen = true
				width, height = rootSize(t)
			}
			// SVG element and attribute names are case-sensitive (linearGradient, viewBox); match case-insensitively
			// but emit the original spelling.
			buf.WriteByte('<')
			buf.WriteString(t.Name.Local)
			for _, a := range t.Attr {
				if qn, val, ok := safeAttr(lname, a); ok {
					buf.WriteByte(' ')
					buf.WriteString(qn)
					buf.WriteString(`="`)
					_ = xml.EscapeText(&buf, []byte(val))
					buf.WriteByte('"')
				}
			}
			buf.WriteByte('>')
		case xml.EndElement:
			if skipDepth > 0 {
				skipDepth--
				continue
			}
			if !allowedElements[strings.ToLower(local(t.Name))] {
				continue
			}
			buf.WriteString("</")
			buf.WriteString(t.Name.Local)
			buf.WriteByte('>')
		case xml.CharData:
			if skipDepth == 0 && rootSeen {
				_ = xml.EscapeText(&buf, t)
			}
		default:
			// Comments, directives (DOCTYPE), and processing instructions are dropped.
		}
	}
	if !rootSeen {
		return nil, 0, 0, fmt.Errorf("%w: no svg element", ErrUnsafeSVG)
	}
	return buf.Bytes(), width, height, nil
}

func local(n xml.Name) string { return n.Local }

// safeAttr returns the attribute's qualified name and value when it is allowed, else ok=false. xmlns declarations and
// local-only href references are kept; event handlers, remote references and style are not.
func safeAttr(element string, a xml.Attr) (qname, value string, ok bool) {
	lname := strings.ToLower(a.Name.Local)
	space := strings.ToLower(a.Name.Space)

	// Keep namespace declarations so the root renders; drop any that bind a scripting namespace.
	if space == "xmlns" {
		if unsafeValue(a.Value) {
			return "", "", false
		}
		return "xmlns:" + a.Name.Local, a.Value, true
	}
	if lname == "xmlns" && a.Name.Space == "" {
		if unsafeValue(a.Value) {
			return "", "", false
		}
		return "xmlns", a.Value, true
	}

	// href / xlink:href only when it is a same-document fragment (#id).
	if lname == "href" {
		v := strings.TrimSpace(a.Value)
		if !strings.HasPrefix(v, "#") {
			return "", "", false
		}
		if space == "xmlns" || strings.Contains(space, "xlink") || a.Name.Space == "xlink" {
			return "xlink:href", v, true
		}
		return "href", v, true
	}

	// Event handlers and anything with a scripting-looking value are refused.
	if strings.HasPrefix(lname, "on") {
		return "", "", false
	}
	if !allowedAttrs[lname] {
		return "", "", false
	}
	if unsafeValue(a.Value) {
		return "", "", false
	}
	return a.Name.Local, a.Value, true
}

// unsafeValue rejects attribute values that could execute or fetch. A url() is allowed only as a same-document
// fragment, url(#id); any url() that points elsewhere, and any script-like scheme, is refused.
func unsafeValue(v string) bool {
	l := strings.ToLower(v)
	if strings.Contains(l, "javascript:") || strings.Contains(l, "@import") ||
		strings.Contains(l, "expression(") || strings.Contains(l, "data:text/html") {
		return true
	}
	for i := 0; ; {
		j := strings.Index(l[i:], "url(")
		if j < 0 {
			return false
		}
		rest := strings.TrimLeft(l[i+j+len("url("):], " \t'\"")
		if !strings.HasPrefix(rest, "#") {
			return true
		}
		i = i + j + len("url(")
	}
}

func rootSize(t xml.StartElement) (w, h float64) {
	var vbW, vbH float64
	for _, a := range t.Attr {
		switch strings.ToLower(a.Name.Local) {
		case "width":
			w = numeric(a.Value)
		case "height":
			h = numeric(a.Value)
		case "viewbox":
			f := strings.FieldsFunc(a.Value, func(r rune) bool { return r == ' ' || r == ',' })
			if len(f) == 4 {
				vbW = numeric(f[2])
				vbH = numeric(f[3])
			}
		}
	}
	if w == 0 {
		w = vbW
	}
	if h == 0 {
		h = vbH
	}
	return w, h
}

// numeric reads a leading number, ignoring a unit such as px or pt.
func numeric(s string) float64 {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			end++
			continue
		}
		break
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0
	}
	return f
}
