package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

const theURL = "https://raw.githubusercontent.com/github/docs/main/content/actions/writing-workflows/choosing-when-your-workflow-runs/events-that-trigger-workflows.md"

var dbg = log.New(io.Discard, "", log.LstdFlags)

// `Node.Text` method was deprecated. This is alternative to it.
// https://github.com/yuin/goldmark/issues/471
func textOf(n ast.Node, src []byte) string {
	var b strings.Builder

	ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if t, ok := n.(*ast.Text); ok {
			b.Write(t.Value(src))
		}
		return ast.WalkContinue, nil
	})

	return b.String()
}

func getFirstLinkText(n ast.Node, src []byte) (string, bool) {
	var link *ast.Link
	ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		if l, ok := n.(*ast.Link); ok {
			link = l
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})

	if link == nil {
		return "", false
	}

	// Note: All text pieces must be collected. For example the text "pull_request" is pieces of
	// "pull_" and "request" since an underscore is delimiter of italic/bold text.
	return textOf(link, src), true
}

func collectCodeSpans(n ast.Node, src []byte) []string {
	spans := []string{}
	ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		kind := n.Kind()
		if entering && kind == ast.KindCodeSpan {
			spans = append(spans, textOf(n, src))
		}
		return ast.WalkContinue, nil
	})
	return spans
}

func getWebhookTypes(table ast.Node, src []byte) ([]string, bool, error) {
	dbg.Println("Table:", textOf(table, src))

	sawHeader := false
	for c := table.FirstChild(); c != nil; c = c.NextSibling() {
		kind := c.Kind()

		if kind == extast.KindTableHeader {
			sawHeader = true

			cell := c.FirstChild()
			if textOf(cell, src) != "Webhook event payload" {
				dbg.Println("  Skip this table because it is not for Webhook event payload")
				return nil, false, nil
			}

			dbg.Println("  Found table header for Webhook event payload")
			continue
		}

		if kind == extast.KindTableRow {
			if !sawHeader {
				dbg.Println("  Skip this table because it does not have a header")
				return nil, false, nil // Unreachable because table without header cannot be written in GFM
			}

			dbg.Println("  Found the first table row")

			// First cell of first row
			cell := c.FirstChild()
			name, ok := getFirstLinkText(cell, src)
			if !ok {
				return nil, false, fmt.Errorf("\"Webhook event payload\" table was found, but first cell did not contain hook name: %q", textOf(cell, src))
			}

			// Second cell
			cell = cell.NextSibling()
			types := collectCodeSpans(cell, src)

			dbg.Printf("  Found Webhook table: %q %v", name, types)
			return types, true, nil
		}
	}

	dbg.Printf("  Table row was not found (sawHeader=%v)", sawHeader)
	return nil, false, nil
}

func generate(src []byte, out io.Writer) error {
	md := goldmark.New(goldmark.WithExtensions(extension.Table))
	root := md.Parser().Parse(text.NewReader(src))

	buf := &bytes.Buffer{}
	fmt.Fprintln(buf, `// Code generated by actionlint/scripts/generate-webhook-events. DO NOT EDIT.

package actionlint

// AllWebhookTypes is a table of all webhooks with their types. This variable was generated by
// script at ./scripts/generate-webhook-events based on
// https://github.com/github/docs/blob/main/content/actions/using-workflows/events-that-trigger-workflows.md
var AllWebhookTypes = map[string][]string {`)

	skipped := []string{
		"schedule",
		"workflow_call",
	}

	numHooks := 0
	sawAbout := false
	currentHook := ""
Toplevel:
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		k := n.Kind()
		if !sawAbout {
			// When '## About events that trigger workflows'
			if h, ok := n.(*ast.Heading); ok && h.Level == 2 && textOf(h, src) == "About events that trigger workflows" {
				sawAbout = true
				dbg.Println("Found \"About events that trigger workflows\" heading")
			}
			continue
		}

		if h, ok := n.(*ast.Heading); ok && h.Level == 2 {
			currentHook = textOf(h, src)
			dbg.Printf("Found new hook %q\n", currentHook)
			continue
		}

		if k != extast.KindTable {
			continue
		}

		for _, h := range skipped {
			if h == currentHook {
				continue Toplevel
			}
		}

		ts, ok, err := getWebhookTypes(n, src)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		numHooks++

		if len(ts) == 0 {
			fmt.Fprintf(buf, "\t%q: {},\n", currentHook)
			continue
		}
		fmt.Fprintf(buf, "\t%q: {%q", currentHook, ts[0])
		for _, t := range ts[1:] {
			fmt.Fprintf(buf, ", %q", t)
		}
		fmt.Fprintln(buf, "},")
	}
	fmt.Fprintln(buf, "}")

	if !sawAbout {
		return errors.New("\"## About events that trigger workflows\" heading was missing")
	}

	if numHooks == 0 {
		return errors.New("no webhook table was found in given markdown source")
	}

	src, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("could not format Go source: %w", err)
	}

	if _, err := out.Write(src); err != nil {
		return fmt.Errorf("could not write output: %w", err)
	}

	return nil
}

func fetch(url string) ([]byte, error) {
	var c http.Client

	dbg.Println("Fetching", url)

	res, err := c.Get(url)
	if err != nil {
		return nil, fmt.Errorf("could not fetch %s: %w", url, err)
	}
	if res.StatusCode < 200 || 300 <= res.StatusCode {
		return nil, fmt.Errorf("request was not successful for %s: %s", url, res.Status)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("could not fetch body for %s: %w", url, err)
	}
	res.Body.Close()

	dbg.Printf("Fetched %d bytes from %s", len(body), url)
	return body, nil
}

func run(args []string, stdout, stderr, dbgout io.Writer, srcURL string) int {
	dbg.SetOutput(dbgout)

	if len(args) > 2 {
		fmt.Fprintln(stderr, "usage: generate-webhook-events events-that-trigger-workflows.md [[srcfile] dstfile]")
		return 1
	}

	dbg.Println("Start generate-webhook-events script")

	var src []byte
	var err error
	if len(args) == 2 {
		src, err = os.ReadFile(args[0])
	} else {
		src, err = fetch(srcURL)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var out io.Writer
	var dst string
	if len(args) == 0 || args[len(args)-1] == "-" {
		out = stdout
		dst = "stdout"
	} else {
		n := args[len(args)-1]
		f, err := os.Create(n)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		out = f
		dst = n
	}

	if err := generate(src, out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	dbg.Println("Wrote output to", dst)
	dbg.Println("Done generate-webhook-events script successfully")

	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Stderr, theURL))
}
