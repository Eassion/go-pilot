---
name: pdf
description: Process PDF files with Go tools only - extract text, create PDFs, merge, split, and inspect metadata.
---

# PDF Processing Skill

Use Go-native tools and libraries only. Do not use Python commands.

## Tooling Setup (Go)

```bash
# Go CLI for merge/split/inspect/validate
go install github.com/pdfcpu/pdfcpu/cmd/pdfcpu@latest
```

## Reading PDFs (Go)

Use `github.com/ledongthuc/pdf` for text extraction:

```go
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
)

func main() {
	f, r, err := pdf.Open("input.pdf")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	totalPage := r.NumPage()
	fmt.Printf("Pages: %d\n", totalPage)
	var b strings.Builder

	for i := 1; i <= totalPage; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			panic(err)
		}
		fmt.Printf("--- Page %d ---\n%s\n", i, text)
		b.WriteString(text)
		b.WriteString("\n")
	}

	if err := os.WriteFile("output.txt", []byte(b.String()), 0o644); err != nil {
		panic(err)
	}
}
```

## Creating PDFs (Go)

Use `github.com/phpdave11/gofpdf`:

```go
package main

import "github.com/phpdave11/gofpdf"

func main() {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Arial", "", 14)
	pdf.Cell(40, 10, "Hello, PDF from Go")
	if err := pdf.OutputFileAndClose("output.pdf"); err != nil {
		panic(err)
	}
}
```

## Merging and Splitting PDFs (Go CLI)

```bash
# Merge
pdfcpu merge merged.pdf file1.pdf file2.pdf file3.pdf

# Split by page
pdfcpu split input.pdf out/
```

## Metadata and Validation (Go CLI)

```bash
# Show metadata/info
pdfcpu info input.pdf

# Validate PDF structure
pdfcpu validate input.pdf
```

## Go Dependencies

| Task | Library | Install |
|------|---------|---------|
| Read text | `ledongthuc/pdf` | `go get github.com/ledongthuc/pdf` |
| Create PDF | `phpdave11/gofpdf` | `go get github.com/phpdave11/gofpdf` |
| Merge/Split/Inspect | `pdfcpu` | `go install github.com/pdfcpu/pdfcpu/cmd/pdfcpu@latest` |

## Best Practices

1. Check tool availability (`pdfcpu version`) before execution.
2. Process large documents page-by-page to control memory.
3. Separate extraction result from logs for deterministic downstream parsing.
4. If extracted text is empty, explicitly report likely scanned-PDF/OCR limitation.
