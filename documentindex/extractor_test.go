package documentindex

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/store"
)

func TestExtractPlainTextAndSplitAllContent(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("aa", "file")
	if err := os.MkdirAll(filepath.Join(root, "aa"), 0o700); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("公司资料和项目事实。", 300)
	if err := os.WriteFile(filepath.Join(root, rel), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := extract(context.Background(), root, store.File{
		OriginalName: "facts.txt", MIMEType: "text/plain", StoragePath: rel,
	})
	if err != nil || got.Extractor != "plain-text" || !strings.Contains(got.Text, "公司资料") {
		t.Fatalf("extract = %+v, %v", got, err)
	}
	chunks := splitText(got.Text)
	if len(chunks) < 2 || !strings.Contains(chunks[len(chunks)-1], "项目事实") {
		t.Fatalf("chunks=%d last=%q", len(chunks), chunks[len(chunks)-1])
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > maxChunkRunes {
			t.Fatalf("chunk too large: %d", len([]rune(chunk)))
		}
	}
}

func TestExtractOfficeXML(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("bb", "book")
	if err := os.MkdirAll(filepath.Join(root, "bb"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("xl/sharedStrings.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<sst><si><t>黄桑</t></si><si><t>产品经理</t></si></sst>`))
	w, err = zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<worksheet><sheetData><row><c><v>13800138000</v></c></row></sheetData></worksheet>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := extract(context.Background(), root, store.File{
		OriginalName: "roster.xlsx", MIMEType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", StoragePath: rel,
	})
	if err != nil || got.Extractor != "office-xml" {
		t.Fatalf("extract = %+v, %v", got, err)
	}
	for _, want := range []string{"黄桑", "产品经理", "13800138000"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("missing %q in %q", want, got.Text)
		}
	}
}

func TestUnsupportedBinaryIsExplicit(t *testing.T) {
	_, err := extract(context.Background(), t.TempDir(), store.File{OriginalName: "video.mp4", MIMEType: "video/mp4"})
	if err == nil || !isUnsupported(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractHonorsCanceledContext(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "plain")
	if err := os.WriteFile(path, []byte(strings.Repeat("content", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := extract(ctx, root, store.File{OriginalName: "facts.txt", MIMEType: "text/plain", StoragePath: "plain"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestCopyRegularToTempRejectsActualSizeBeyondLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "oversized"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := copyRegularToTemp(context.Background(), root, store.File{
		OriginalName: "scan.png", StoragePath: "oversized",
	}, 4)
	cleanup()
	if !errors.Is(err, ErrUnsafeInput) {
		t.Fatalf("err = %v", err)
	}
}

func TestPDFPageNumberUsesNumericSuffix(t *testing.T) {
	second, secondOK := pdfPageNumber("/tmp/page-2.png")
	tenth, tenthOK := pdfPageNumber("/tmp/page-10.png")
	if !secondOK || !tenthOK || second != 2 || tenth != 10 || second >= tenth {
		t.Fatalf("page numbers = (%d,%t) (%d,%t)", second, secondOK, tenth, tenthOK)
	}
	if _, ok := pdfPageNumber("/tmp/page.png"); ok {
		t.Fatal("page without numeric suffix must not parse")
	}
}

func TestOfficeArchiveRejectsExcessiveTextEntries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "too-many")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for i := 0; i <= maxOfficeEntries; i++ {
		entry, createErr := zw.Create(fmt.Sprintf("xl/worksheets/sheet%05d.xml", i))
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := entry.Write([]byte(`<worksheet><v>1</v></worksheet>`)); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = extract(context.Background(), root, store.File{
		OriginalName: "too-many.xlsx", MIMEType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", StoragePath: "too-many",
	})
	if !errors.Is(err, ErrUnsafeInput) || !isTerminalExtractionError(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestScannedPDFFallsBackToOCR(t *testing.T) {
	bin := t.TempDir()
	writeTestCommand(t, bin, "pdftotext", "#!/bin/sh\nexit 0\n")
	writeTestCommand(t, bin, "pdftoppm", `#!/bin/sh
for arg in "$@"; do prefix="$arg"; done
printf 'image' > "${prefix}-1.png"
`)
	writeTestCommand(t, bin, "tesseract", `#!/bin/sh
if [ "$1" = "--list-langs" ]; then
  printf 'eng\n'
else
  printf '扫描件中的公司资料\n'
fi
`)
	t.Setenv("PATH", bin)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scan"), []byte("%PDF-fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := extract(context.Background(), root, store.File{
		OriginalName: "scan.pdf", MIMEType: "application/pdf", StoragePath: "scan",
	})
	if err != nil || got.Extractor != "pdf-ocr" || !strings.Contains(got.Text, "公司资料") {
		t.Fatalf("extract = %+v, %v", got, err)
	}
}

func writeTestCommand(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}
