package compiler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type extractedSource struct {
	Text      []byte
	RawName   string
	Extractor string
	SourceExt string
}

func extractSourceText(sourcePath string, data []byte) (extractedSource, error) {
	ext := strings.ToLower(filepath.Ext(sourcePath))
	switch ext {
	case ".txt", ".md", "":
		return extractedSource{Text: data, RawName: filepath.Base(sourcePath), Extractor: "direct", SourceExt: ext}, nil
	case ".docx":
		text, err := extractDOCXText(data)
		if err != nil {
			return extractedSource{}, err
		}
		return extractedSource{Text: renderExtractedMarkdown(sourcePath, text), RawName: extractedRawMarkdownName(sourcePath), Extractor: "docx-xml", SourceExt: ext}, nil
	case ".pdf":
		text, err := extractPDFText(sourcePath)
		if err != nil {
			return extractedSource{}, err
		}
		return extractedSource{Text: renderExtractedMarkdown(sourcePath, text), RawName: extractedRawMarkdownName(sourcePath), Extractor: "pdftotext", SourceExt: ext}, nil
	default:
		return extractedSource{}, fmt.Errorf("unsupported source extension %q; supported: .txt, .md, .pdf, .docx", ext)
	}
}

func extractDOCXText(data []byte) (string, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("read docx: %w", err)
	}
	var document *zip.File
	for _, file := range reader.File {
		if file.Name == "word/document.xml" {
			document = file
			break
		}
	}
	if document == nil {
		return "", fmt.Errorf("read docx: word/document.xml not found")
	}
	rc, err := document.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	decoder := xml.NewDecoder(rc)
	var b strings.Builder
	var inText bool
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch value := token.(type) {
		case xml.StartElement:
			name := value.Name.Local
			if name == "t" {
				inText = true
			}
			if name == "tab" {
				b.WriteByte('\t')
			}
			if name == "br" {
				b.WriteByte('\n')
			}
		case xml.EndElement:
			name := value.Name.Local
			if name == "t" {
				inText = false
			}
			if name == "p" {
				b.WriteString("\n\n")
			}
		case xml.CharData:
			if inText {
				b.Write([]byte(value))
			}
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", fmt.Errorf("read docx: extracted text is empty")
	}
	return text, nil
}

func extractPDFText(sourcePath string) (string, error) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		return "", fmt.Errorf("read pdf: pdftotext is required for local PDF extraction")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", sourcePath, "-")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("read pdf: pdftotext timed out")
	}
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("read pdf: extracted text is empty")
	}
	return text, nil
}

func renderExtractedMarkdown(sourcePath, text string) []byte {
	title := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "> Extracted from immutable original `%s` for LLM Wiki ingestion.\n\n", filepath.Base(sourcePath))
	b.WriteString(strings.TrimSpace(text))
	b.WriteByte('\n')
	return []byte(b.String())
}

func extractedRawMarkdownName(sourcePath string) string {
	base := filepath.Base(sourcePath)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext) + ".md"
}
