// assemble builds a deployable TPS template from the cleaned base HTML.
//
// It replaces the %HEAD%, %TITLE%, %PAGE-TITLE%, and %BODY% markers in
// base.html with per-page substitution files, then splices sb.css back in as
// an inline <style> block (TPS cannot serve static files, so the final
// template must be self-contained).
//
// The tool fails loudly rather than produce a silently broken page: every
// input file must exist (empty is fine, missing is not), every marker must
// appear in base.html, and no %MARKER% may survive into the output. All
// problems are reported at once so one run shows everything to fix.
//
// Marker substitution happens before the CSS splice, so the CSS contents can
// never be mangled by a coincidental %WORD% inside them. The Go template
// actions ({{.SiteKey}} etc.) in the substitution files pass through
// verbatim; nothing here interprets them.
//
// Usage: assemble -dir tps-templates -page challenge
//
// Reads <dir>/base.html, <dir>/sb.css, <dir>/<page>.head.html,
// <dir>/<page>.body.html, <dir>/<page>.title.txt, and
// <dir>/<page>.page-title.txt, and writes <dir>/<page>.go.html.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// cssLink is the tag tools/htmlfmt writes in place of the first <style>
// block; the two tools must agree on it exactly.
const cssLink = `<link rel="stylesheet" href="sb.css">`

// markers maps each %MARKER% in base.html to its per-page substitution file
// suffix. Text values get whitespace-trimmed (editors add trailing newlines);
// HTML fragments are inserted verbatim. %PAGE-TITLE% is listed before
// %TITLE% so a future token can safely contain another as a substring.
var markers = []struct {
	token  string
	suffix string
	trim   bool
}{
	{"%HEAD%", "head.html", false},
	{"%BODY%", "body.html", false},
	{"%PAGE-TITLE%", "page-title.txt", true},
	{"%TITLE%", "title.txt", true},
}

var leftoverMarker = regexp.MustCompile(`%[A-Z][A-Z-]*%`)

func main() {
	dir := flag.String("dir", "", `directory holding base.html, sb.css, and the substitution files`)
	page := flag.String("page", "", `page to assemble, e.g. "challenge" or "failed"`)
	flag.Parse()
	if *dir == "" || *page == "" {
		fmt.Fprintln(os.Stderr, "assemble: -dir and -page must both be specified")
		os.Exit(1)
	}

	var problems []string
	complain := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	base, err := os.ReadFile(filepath.Join(*dir, "base.html"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "assemble:", err)
		os.Exit(1)
	}

	// Substitute every marker; a missing marker or substitution file is a
	// problem, but keep going so the user sees the full list.
	for _, m := range markers {
		pth := filepath.Join(*dir, *page+"."+m.suffix)
		val, err := os.ReadFile(pth)
		if err != nil {
			complain("cannot read %s (create it even if empty): %s", pth, err)
			continue
		}
		if m.trim {
			val = bytes.TrimSpace(val)
		}
		if !bytes.Contains(base, []byte(m.token)) {
			complain("marker %s not found in base.html (see the README's cleanup steps)", m.token)
			continue
		}
		base = bytes.ReplaceAll(base, []byte(m.token), val)
	}

	// Anything still marker-shaped is a typo in base.html or a marker we
	// don't know about — either way the page would ship broken.
	if leftovers := leftoverMarker.FindAll(base, -1); leftovers != nil {
		seen := map[string]bool{}
		for _, l := range leftovers {
			if s := string(l); !seen[s] {
				seen[s] = true
				complain("unreplaced marker %s in the assembled output", s)
			}
		}
	}

	// Splice the CSS back in where htmlfmt put the link. Exactly one link:
	// zero means the cleanup lost it, more than one means something odd
	// happened and we'd rather a human look than duplicate 2.6MB of CSS.
	switch n := bytes.Count(base, []byte(cssLink)); n {
	case 1:
		css, err := os.ReadFile(filepath.Join(*dir, "sb.css"))
		if err != nil {
			complain("cannot read the extracted CSS: %s", err)
			break
		}
		if len(css) > 0 && css[len(css)-1] != '\n' {
			css = append(css, '\n')
		}
		var style bytes.Buffer
		style.WriteString("<style>\n")
		style.Write(css)
		style.WriteString("</style>")
		base = bytes.Replace(base, []byte(cssLink), style.Bytes(), 1)
	case 0:
		complain("base.html does not contain %s — it must appear exactly as htmlfmt wrote it", cssLink)
	default:
		complain("base.html contains %s %d times; expected exactly one", cssLink, n)
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "assemble:", p)
		}
		os.Exit(1)
	}

	out := filepath.Join(*dir, *page+".go.html")
	if err := os.WriteFile(out, base, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "assemble:", err)
		os.Exit(1)
	}
	fmt.Printf("assemble: wrote %s (%d bytes)\n", out, len(base))
}
