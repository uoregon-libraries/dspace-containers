// htmlfmt indents HTML by inserting line breaks and indentation, and does
// nothing else: every token is re-emitted byte-for-byte from the input.
//
// Breaks are only inserted before block-level tags, where HTML collapses
// whitespace to nothing, so formatting can never change how a page renders.
// Inline elements (<a>, <span>, <i>, ...) stay glued to their neighbors, and
// the contents of <pre>, <script>, <style>, and <textarea> are never touched.
// Custom elements (e.g. Angular's ds-*) are treated as block: whitespace
// around one in an inline context *can* render, but breaking them onto their
// own lines is what makes the output editable, and the few whitespace-
// sensitive spots (nav menus) get deleted during template cleanup anyway.
//
// The contents of all <style> blocks are moved verbatim to sb.css, written
// next to the output file, and a single <link rel="stylesheet"> to it
// replaces the first block. All blocks are concatenated in document order, so
// the CSS cascade is unchanged. The name sb.css is fixed: tools/assemble
// expects exactly that link when it re-inlines the CSS.
//
// Usage: htmlfmt -output out.html [-raw in.html]
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/net/html"
)

const indent = "  "

// Elements rendered inline by default; never break around these or their
// hyphenated custom-element cousins.
var inlineTags = map[string]bool{
	"a": true, "abbr": true, "audio": true, "b": true, "bdi": true,
	"bdo": true, "br": true, "button": true, "canvas": true, "cite": true,
	"code": true, "data": true, "del": true, "dfn": true, "em": true,
	"embed": true, "i": true, "iframe": true, "img": true, "input": true,
	"ins": true, "kbd": true, "label": true, "map": true, "mark": true,
	"meter": true, "object": true, "output": true, "picture": true,
	"progress": true, "q": true, "s": true, "samp": true, "select": true,
	"small": true, "span": true, "strong": true, "sub": true, "sup": true,
	"svg": true, "time": true, "u": true, "var": true, "video": true,
	"wbr": true,
}

// Void elements never get a closing tag, so they don't affect nesting depth.
var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// Whitespace inside these is significant (or is code, or is the page title);
// leave it alone.
var rawTags = map[string]bool{
	"pre": true, "script": true, "style": true, "textarea": true, "title": true,
}

func isBlock(name string) bool {
	return !inlineTags[name]
}

func main() {
	rawPath := flag.String("raw", "", `input HTML file (default stdin)`)
	outPath := flag.String("output", "", `output HTML file`)
	flag.Parse()

	inFile := os.Stdin
	if *rawPath != "" {
		f, err := os.Open(*rawPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "htmlfmt:", err)
			os.Exit(1)
		}
		defer f.Close()
		inFile = f
	}

	if *outPath == "" {
		fmt.Fprintln(os.Stderr, "htmlfmt: -output must be specified")
		os.Exit(1)
	}
	cssPath := filepath.Join(filepath.Dir(*outPath), "sb.css")

	outFile, err := os.Create(*outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "htmlfmt:", err)
		os.Exit(1)
	}
	defer outFile.Close()

	in := bufio.NewReader(inFile)
	out := bufio.NewWriter(outFile)
	defer out.Flush()

	z := html.NewTokenizer(in)
	var line bytes.Buffer
	var css bytes.Buffer
	var stack []string // open block elements, for depth + implied end tags
	rawDepth := 0
	inStyle := false
	sawStyle := false

	// Ends the current output line before a block boundary. Trailing
	// whitespace (and whitespace-only lines) collapse to nothing next to a
	// block edge, so trimming or dropping them cannot affect rendering.
	flush := func(depth int) {
		if trimmed := bytes.TrimRight(line.Bytes(), " \t\r\n"); len(trimmed) > 0 {
			out.Write(trimmed)
			out.WriteByte('\n')
		}
		line.Reset()
		for range depth {
			line.WriteString(indent)
		}
	}

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		raw := z.Raw()
		name := ""
		if tt == html.StartTagToken || tt == html.EndTagToken || tt == html.SelfClosingTagToken {
			b, _ := z.TagName()
			name = string(b)
		}

		// Style extraction: swallow the <style> tags and divert their text
		// to the css buffer; the first block becomes a <link> to the file.
		if rawDepth == 0 {
			switch {
			case tt == html.StartTagToken && name == "style":
				flush(len(stack))
				if !sawStyle {
					fmt.Fprintf(&line, `<link rel="stylesheet" href=%q>`, filepath.Base(cssPath))
				}
				inStyle, sawStyle = true, true
				continue
			case inStyle && tt == html.EndTagToken && name == "style":
				if css.Len() > 0 && css.Bytes()[css.Len()-1] != '\n' {
					css.WriteByte('\n')
				}
				inStyle = false
				continue
			case inStyle:
				css.Write(raw)
				continue
			}
		}

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			if rawDepth == 0 && isBlock(name) {
				// A sibling <li> (etc.) implies the previous one closed.
				if len(stack) > 0 && stack[len(stack)-1] == name &&
					(name == "li" || name == "option" || name == "tr" || name == "td" || name == "th") {
					stack = stack[:len(stack)-1]
				}
				flush(len(stack))
			}
			line.Write(raw)
			if tt == html.StartTagToken && isBlock(name) && !voidTags[name] && name != "html" {
				stack = append(stack, name)
			}
			if rawTags[name] {
				rawDepth++
			}
		case html.EndTagToken:
			wasRaw := rawDepth > 0
			if rawTags[name] && rawDepth > 0 {
				rawDepth--
			}
			if isBlock(name) {
				// Pop implied-closed elements until the match; ignore strays.
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i] == name {
						stack = stack[:i]
						break
					}
				}
				if !wasRaw && rawDepth == 0 {
					flush(len(stack))
				}
			}
			line.Write(raw)
		case html.DoctypeToken, html.CommentToken:
			if rawDepth == 0 {
				flush(len(stack))
			}
			line.Write(raw)
		default: // text
			line.Write(raw)
		}
	}
	if err := z.Err(); err != nil && err.Error() != "EOF" {
		fmt.Fprintln(os.Stderr, "htmlfmt:", err)
		os.Exit(1)
	}
	flush(0)

	if sawStyle {
		if err := os.WriteFile(cssPath, css.Bytes(), 0644); err != nil {
			fmt.Fprintln(os.Stderr, "htmlfmt:", err)
			os.Exit(1)
		}
	}
}
