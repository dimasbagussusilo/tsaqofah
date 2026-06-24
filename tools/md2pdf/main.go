// md2pdf converts Markdown file(s) to PDF using a headless Chromium/Chrome.
//
// It renders Markdown to HTML (blackfriday), writes the HTML to a temp file,
// and runs Chrome's built-in --print-to-pdf on it. This is pure process launch
// + file I/O (no DevTools/WebSocket networking), so it works reliably under
// WSL2 even when the browser is Windows Chrome reached via /mnt/c.
//
// Usage:
//
//	md2pdf <file.md> [more.md ... | dir/]
//
// Each input file.md produces file.pdf in the same directory.
//
// The browser is auto-detected; override it with the MD2PDF_CHROME env var.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// htmlTemplate wraps rendered Markdown in an HTML document styled for clean,
// beautiful PDF output (Dillinger-inspired: serif body, sans-serif headings,
// a green accent). The "{{BODY}}" marker is replaced with the HTML blackfriday
// produces. There is a single @page margin and no body padding/centering, so
// the text is not over-indented on the page.
const htmlTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<style>
  /* Stylesheet ported verbatim from Dillinger's PDF/Styled-HTML export
     (lib/export.ts STYLED_EXPORT_CSS) so output matches dillinger.io. */
  /* Page: a single margin (no double body padding) so text isn't over-indented. */
  @page { size: A4; margin: 22mm 22mm 20mm 22mm; }
  * { box-sizing: border-box; }
  body {
    font-family: Cambria, Georgia, "Arabic Typesetting", "Sakkal Majalla",
                 "Noto Naskh Arabic", Tahoma, serif;
    font-size: 11pt;
    line-height: 1.7;
    color: #2b2f33;
    margin: 0;
    text-align: left;
    -webkit-print-color-adjust: exact;
    print-color-adjust: exact;
  }
  /* Clean sans-serif headings, kept with the content that follows them. */
  h1, h2, h3, h4, h5, h6 {
    font-family: "Segoe UI", "Helvetica Neue", Arial, "Arabic Typesetting", sans-serif;
    font-weight: 600;
    line-height: 1.3;
    color: #14483a;
    break-after: avoid-page;
    page-break-after: avoid;
  }
  h1 { font-size: 1.95em; margin: 0 0 .6em; padding-bottom: .3em;
       border-bottom: 2px solid #14483a; letter-spacing: -.01em; }
  h2 { font-size: 1.45em; margin: 1.5em 0 .5em; padding-bottom: .2em;
       border-bottom: 1px solid #d9e2dc; }
  h3 { font-size: 1.18em; margin: 1.3em 0 .4em; color: #1b5e47; }
  h4 { font-size: 1em; margin: 1.1em 0 .35em; color: #1b5e47; }
  p  { margin: 0 0 .85em; orphans: 3; widows: 3; }
  strong { color: #1f2426; }
  /* Lists: tidy, not over-indented. */
  ul, ol { margin: 0 0 .85em; padding-left: 1.4em; }
  li { margin: 0 0 .35em; break-inside: avoid-page; page-break-inside: avoid; }
  li > ul, li > ol { margin: .35em 0 0; }
  a { color: #14633c; text-decoration: none; border-bottom: 1px solid rgba(20,99,60,.3); }
  code {
    font-family: "Cascadia Code", Consolas, "DejaVu Sans Mono", monospace;
    font-size: .88em; color: #1b3a2c; background: #f3f5f4;
    padding: .12em .38em; border-radius: 4px;
  }
  pre {
    background: #f3f5f4; border: 1px solid #e3e8e5; border-left: 3px solid #14633c;
    padding: .9em 1em; border-radius: 6px; overflow-x: auto; line-height: 1.5;
    break-inside: avoid-page; page-break-inside: avoid;
  }
  pre code { background: none; padding: 0; color: inherit; font-size: .85em; }
  blockquote {
    margin: 0 0 .9em; padding: .4em 1em; border-left: 3px solid #14633c;
    background: #f5f8f6; color: #3c4541; font-style: italic;
    break-inside: avoid-page; page-break-inside: avoid;
  }
  table { border-collapse: collapse; width: 100%; margin: 0 0 .9em; font-size: .95em;
          break-inside: avoid-page; page-break-inside: avoid; }
  thead { background: #eef3f0; }
  th, td { border: 1px solid #d9e2dc; padding: .5em .7em; text-align: left; }
  th { font-family: "Segoe UI", Arial, sans-serif; font-weight: 600; color: #23302a; }
  tr { break-inside: avoid-page; page-break-inside: avoid; }
  img { max-width: 100%; height: auto; break-inside: avoid-page; page-break-inside: avoid; }
  hr { border: none; border-top: 1px solid #d9e2dc; margin: 1.6em 0; }
</style>
</head>
<body>
{{BODY}}
</body>
</html>`

// mdConverter renders Markdown to HTML with goldmark (CommonMark + GFM).
// goldmark is used instead of blackfriday because blackfriday absorbs a heading
// that immediately follows a bullet list into the list item, which indents that
// heading — the "A/B and 1./2. not at the same level" bug. WithUnsafe() keeps
// raw HTML (e.g. the inline Arabic-verse <div>) intact.
var mdConverter = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

// mdToHTML renders Markdown bytes to a complete HTML document. It uses a plain
// string substitution (not fmt) so the literal "%" in the CSS stays intact.
func mdToHTML(md []byte) string {
	var buf bytes.Buffer
	if err := mdConverter.Convert(md, &buf); err != nil {
		// Fall back to raw text so we never emit an empty document.
		return strings.Replace(htmlTemplate, "{{BODY}}", string(md), 1)
	}
	return strings.Replace(htmlTemplate, "{{BODY}}", buf.String(), 1)
}

// findChrome locates a Chromium/Chrome executable. It honours MD2PDF_CHROME,
// then checks common Linux locations, then Windows paths reachable from WSL2.
func findChrome() (string, error) {
	if p := os.Getenv("MD2PDF_CHROME"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	candidates := []string{
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/snap/bin/chromium",
		"/opt/google/chrome/chrome",
		// Windows paths, reachable from WSL2 via /mnt/c:
		"/mnt/c/Program Files/Google/Chrome/Application/chrome.exe",
		"/mnt/c/Program Files (x86)/Google/Chrome/Application/chrome.exe",
		"/mnt/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Chrome/Chromium found; set MD2PDF_CHROME to its path")
}

// isWindowsExe reports whether the browser path is a Windows .exe under /mnt.
func isWindowsExe(p string) bool {
	return strings.HasPrefix(p, "/mnt/") && strings.HasSuffix(strings.ToLower(p), ".exe")
}

// winDrive splits a /mnt/<drive>/<rest> path into an uppercased drive letter
// (e.g. "C") and the remainder. ok is false for non-/mnt paths.
func winDrive(p string) (drive, rest string, ok bool) {
	if !strings.HasPrefix(p, "/mnt/") {
		return "", "", false
	}
	rel := strings.TrimPrefix(p, "/mnt/") // "c/dbs-codes/..."
	parts := strings.SplitN(rel, "/", 2)
	drive = strings.ToUpper(parts[0])
	if len(parts) == 2 {
		rest = parts[1]
	}
	return drive, rest, true
}

// toWinPath returns a backslash Windows path for a /mnt/<drive>/... path.
func toWinPath(p string) string {
	d, rest, ok := winDrive(p)
	if !ok {
		return p
	}
	return d + `:\` + strings.ReplaceAll(rest, "/", `\`)
}

// toWinFileURL returns a file:/// URL for a /mnt/<drive>/... path.
func toWinFileURL(p string) string {
	d, rest, ok := winDrive(p)
	if !ok {
		return "file://" + p
	}
	return "file:///" + d + ":/" + rest
}

// htmlToPDF writes html to a temp file next to pdfPath (so it stays reachable
// from a Windows browser via /mnt/c), then prints it with headless Chrome.
// profileDir is an isolated --user-data-dir (required so a running, non-headless
// Chrome session doesn't swallow the launch).
func htmlToPDF(ctx context.Context, browser, profileDir, html string, pdfPath string) error {
	// Absolute paths are required: a Windows Chrome launched via WSL cannot
	// resolve relative Linux paths, and the Windows-path conversion below only
	// matches absolute /mnt/<drive>/... paths.
	absPDF, err := filepath.Abs(pdfPath)
	if err != nil {
		return err
	}
	pdfPath = absPDF
	tmpHTML := strings.TrimSuffix(pdfPath, filepath.Ext(pdfPath)) + ".tmp.html"
	if err := os.WriteFile(tmpHTML, []byte(html), 0o644); err != nil {
		return err
	}
	debug := os.Getenv("MD2PDF_DEBUG") != ""
	if !debug {
		defer os.Remove(tmpHTML)
	}

	var urlArg, pdfArg, profileArg string
	if isWindowsExe(browser) {
		urlArg = toWinFileURL(tmpHTML)
		pdfArg = toWinPath(pdfPath)
		profileArg = toWinPath(profileDir)
	} else {
		urlArg = "file://" + tmpHTML
		pdfArg = pdfPath
		profileArg = profileDir
	}

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-pdf-header-footer",
		"--user-data-dir=" + profileArg,
		"--print-to-pdf=" + pdfArg,
		urlArg,
	}
	if debug {
		fmt.Fprintf(os.Stderr, "  [debug] %s %s\n", browser, strings.Join(args, " "))
	}
	cmd := exec.CommandContext(ctx, browser, args...)
	out, _ := cmd.CombinedOutput()
	// Chrome (especially on Windows via WSL) often exits non-zero — e.g. from a
	// reused profile's singleton lock — yet still writes the PDF, and that file
	// only becomes visible on the /mnt mount a moment later. So judge success by
	// the output file's presence rather than the process exit code.
	if !waitForFile(pdfPath, 6*time.Second) {
		hint := ""
		if _, err := os.Stat(pdfPath); err == nil {
			hint = " (file exists but may be locked — close any viewer holding it)"
		} else {
			hint = " — close any viewer holding the file and retry"
		}
		return fmt.Errorf("pdf not written%s: %s", hint, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitForFile returns true once path exists, polling until the timeout. It
// tolerates the cross-filesystem visibility delay under WSL.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// collectInputs expands the CLI args into a list of *.md file paths, expanding
// directories to their *.md contents.
func collectInputs(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		info, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			matches, err := filepath.Glob(filepath.Join(a, "*.md"))
			if err != nil {
				return nil, err
			}
			out = append(out, matches...)
			continue
		}
		if !strings.HasSuffix(strings.ToLower(a), ".md") {
			return nil, fmt.Errorf("not a markdown file: %s", a)
		}
		out = append(out, a)
	}
	return out, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <file.md> [more.md ... | dir/]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}

	inputs, err := collectInputs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "inputs:", err)
		os.Exit(1)
	}
	if len(inputs) == 0 {
		fmt.Fprintln(os.Stderr, "no markdown files to convert")
		os.Exit(1)
	}

	browser, err := findChrome()
	if err != nil {
		fmt.Fprintln(os.Stderr, "find chrome:", err)
		os.Exit(1)
	}

	// A fresh --user-data-dir is created per file so a running, non-headless
	// Chrome doesn't intercept the launch and so repeated launches don't trip
	// over each other's singleton lock. For a Windows browser under WSL it must
	// live under /mnt so the .exe can reach it; otherwise the OS temp is fine.
	baseTemp := ""
	if isWindowsExe(browser) {
		baseTemp = "/mnt/c"
	}

	failed := 0
	for _, in := range inputs {
		out := strings.TrimSuffix(in, filepath.Ext(in)) + ".pdf"
		md, err := os.ReadFile(in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", in, err)
			failed++
			continue
		}
		profileDir, err := os.MkdirTemp(baseTemp, "md2pdf-profile-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: temp profile: %v\n", in, err)
			failed++
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		err = htmlToPDF(ctx, browser, profileDir, mdToHTML(md), out)
		cancel()
		os.RemoveAll(profileDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", in, err)
			failed++
			continue
		}
		info, _ := os.Stat(out)
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		fmt.Printf("  ✓ %s -> %s (%d bytes)\n", in, out, size)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d file(s) failed\n", failed)
		os.Exit(1)
	}
}
