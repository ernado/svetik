// Package scraper implements lilith.Scraper on top of a headless Chrome (see
// Browser). This file holds the shared HTML-extraction helpers used to turn a
// rendered page into a lilith.ScrapeResult.
package scraper

import (
	"io"
	"strings"

	"github.com/go-faster/errors"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/ernado/lilith"
)

// extractContent parses HTML from r (which must already be UTF-8) and fills the
// result's Title, Description and Text.
func extractContent(r io.Reader, result *lilith.ScrapeResult) error {
	doc, err := html.Parse(r)
	if err != nil {
		return errors.Wrap(err, "parse html")
	}

	e := &extractor{result: result}
	e.walk(doc, false)

	// Fall back to og:title/description when no title element was present.
	if result.Title == "" {
		result.Title = e.ogTitle
	}
	if result.Description == "" {
		result.Description = e.ogDescription
	}
	result.Text = normalizeWhitespace(e.text.String())

	return nil
}

// blockElements are rendered on their own line so extracted text keeps a sense
// of paragraph separation.
var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Br: true, atom.Li: true,
	atom.Ul: true, atom.Ol: true, atom.Tr: true, atom.Section: true,
	atom.Article: true, atom.Header: true, atom.Footer: true,
	atom.Blockquote: true, atom.Pre: true, atom.Hr: true,
	atom.H1: true, atom.H2: true, atom.H3: true,
	atom.H4: true, atom.H5: true, atom.H6: true,
}

// extractor accumulates page text and metadata while walking the parse tree.
type extractor struct {
	result        *lilith.ScrapeResult
	text          strings.Builder
	ogTitle       string
	ogDescription string
}

func (e *extractor) walk(n *html.Node, inBody bool) {
	switch n.Type {
	case html.ElementNode:
		switch n.DataAtom {
		case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg, atom.Iframe:
			// Non-content subtrees: skip entirely.
			return
		case atom.Title:
			if t := strings.TrimSpace(textContent(n)); t != "" {
				e.result.Title = t
			}
			return
		case atom.Meta:
			e.handleMeta(n)
			return
		case atom.Body:
			inBody = true
		}

		if inBody && blockElements[n.DataAtom] {
			e.text.WriteByte('\n')
		}
	case html.TextNode:
		if inBody {
			e.text.WriteString(n.Data)
			e.text.WriteByte(' ')
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		e.walk(c, inBody)
	}
}

// handleMeta records description and Open Graph fallbacks from a <meta> tag.
func (e *extractor) handleMeta(n *html.Node) {
	var name, property, content string
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "name":
			name = strings.ToLower(a.Val)
		case "property":
			property = strings.ToLower(a.Val)
		case "content":
			content = a.Val
		}
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	switch {
	case name == "description" && e.result.Description == "":
		e.result.Description = content
	case property == "og:description" && e.ogDescription == "":
		e.ogDescription = content
	case property == "og:title" && e.ogTitle == "":
		e.ogTitle = content
	}
}

// textContent returns the concatenated text of a node's descendants.
func textContent(n *html.Node) string {
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			sb.WriteString(c.Data)
		case html.ElementNode:
			sb.WriteString(textContent(c))
		}
	}

	return sb.String()
}

// normalizeWhitespace collapses intra-line whitespace, drops blank lines, and
// joins the remaining lines with single newlines.
func normalizeWhitespace(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			out = append(out, line)
		}
	}

	return strings.Join(out, "\n")
}
