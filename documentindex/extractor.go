// Package documentindex extracts deterministic searchable text from immutable
// files. It does not interpret instructions, create knowledge, or call the chat
// model; its only job is to make already-authorized content discoverable.
package documentindex

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zdypro888/nbco/safefs"
	"github.com/zdypro888/nbco/store"
)

const (
	maxExtractedBytes       = 16 << 20
	maxOfficeArchiveBytes   = 256 << 20
	maxOfficeEntryBytes     = 64 << 20
	maxOfficeInflatedBytes  = 128 << 20
	maxOfficeEntries        = 4000
	maxOfficeArchiveEntries = 10000
	maxPDFInputBytes        = 256 << 20
	maxOCRImageBytes        = 64 << 20
	maxOCRTotalBytes        = 512 << 20
	maxOCRPages             = 40
	maxChunkRunes           = 1600
	chunkOverlapRunes       = 160
	capabilityCacheTTL      = 5 * time.Minute
)

var ErrUnsupported = errors.New("没有可用的确定性文本提取器")
var ErrUnsafeInput = errors.New("文件解压或转换规模超过安全限制")
var errExtractionLimit = errors.New("extracted text limit reached")

type extractorCapabilities struct {
	revision          string
	tesseractLanguage string
}

var extractorCapabilityCache struct {
	sync.Mutex
	at    time.Time
	value extractorCapabilities
}

type extraction struct {
	Text      string
	Extractor string
	Truncated bool
}

func extract(ctx context.Context, root string, file store.File) (extraction, error) {
	ext := strings.ToLower(filepath.Ext(file.OriginalName))
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(file.MIMEType, ";")[0]))
	switch {
	case isPlainText(ext, mimeType):
		return extractPlain(ctx, root, file)
	case ext == ".pdf" || mimeType == "application/pdf":
		return extractPDF(ctx, root, file)
	case isOfficeArchive(ext, mimeType):
		return extractOffice(ctx, root, file, officeKind(ext, mimeType))
	case strings.HasPrefix(mimeType, "image/") || slices.Contains([]string{".png", ".jpg", ".jpeg", ".tif", ".tiff", ".bmp", ".webp"}, ext):
		args := []string{"{file}", "stdout"}
		if language := preferredTesseractLanguage(ctx); language != "" {
			args = append(args, "-l", language)
		}
		return extractCommand(ctx, root, file, "tesseract", args, "tesseract-ocr", maxOCRImageBytes)
	default:
		return extraction{}, fmt.Errorf("%w: mime=%s ext=%s", ErrUnsupported, mimeType, ext)
	}
}

func preferredTesseractLanguage(ctx context.Context) string {
	return loadExtractorCapabilities(ctx).tesseractLanguage
}

func extractorCapabilityRevision(ctx context.Context) string {
	return loadExtractorCapabilities(ctx).revision
}

func loadExtractorCapabilities(ctx context.Context) extractorCapabilities {
	now := time.Now()
	extractorCapabilityCache.Lock()
	if !extractorCapabilityCache.at.IsZero() && now.Sub(extractorCapabilityCache.at) < capabilityCacheTTL {
		value := extractorCapabilityCache.value
		extractorCapabilityCache.Unlock()
		return value
	}
	extractorCapabilityCache.Unlock()

	parts := make([]string, 0, 8)
	for _, command := range []string{"pdftotext", "pdftoppm", "tesseract"} {
		path, err := exec.LookPath(command)
		if err != nil {
			parts = append(parts, command+"=missing")
			continue
		}
		part := command + "=" + path
		if info, statErr := os.Stat(path); statErr == nil {
			part += fmt.Sprintf(":%d:%d", info.Size(), info.ModTime().UnixNano())
		}
		parts = append(parts, part)
	}
	languages := tesseractLanguages(ctx)
	parts = append(parts, "tesseract_languages="+strings.Join(languages, ","))
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	value := extractorCapabilities{
		revision:          fmt.Sprintf("v2:%x", sum[:12]),
		tesseractLanguage: selectTesseractLanguage(languages),
	}
	if ctx.Err() == nil {
		extractorCapabilityCache.Lock()
		extractorCapabilityCache.at = now
		extractorCapabilityCache.value = value
		extractorCapabilityCache.Unlock()
	}
	return value
}

func tesseractLanguages(ctx context.Context) []string {
	if _, err := exec.LookPath("tesseract"); err != nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stdout := newCappedBuffer(64 << 10)
	cmd := exec.CommandContext(probeCtx, "tesseract", "--list-langs")
	cmd.Stdout = stdout
	cmd.Stderr = newCappedBuffer(16 << 10)
	if err := cmd.Run(); err != nil {
		return nil
	}
	languages := strings.Fields(stdout.String())
	sort.Strings(languages)
	return slices.Compact(languages)
}

func selectTesseractLanguage(languages []string) string {
	available := make(map[string]bool)
	for _, language := range languages {
		available[strings.TrimSpace(language)] = true
	}
	switch {
	case available["chi_sim"] && available["eng"]:
		return "chi_sim+eng"
	case available["chi_sim"]:
		return "chi_sim"
	case available["eng"]:
		return "eng"
	default:
		return ""
	}
}

func isPlainText(ext, mimeType string) bool {
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	if slices.Contains([]string{
		"application/json", "application/ld+json", "application/xml",
		"application/x-yaml", "application/yaml", "application/sql",
		"application/javascript", "application/x-ndjson",
	}, mimeType) {
		return true
	}
	return slices.Contains([]string{
		".txt", ".md", ".csv", ".tsv", ".json", ".jsonl", ".ndjson",
		".xml", ".yaml", ".yml", ".log", ".sql", ".go", ".py", ".js",
		".ts", ".tsx", ".jsx", ".java", ".c", ".h", ".cpp", ".hpp",
		".rs", ".rb", ".php", ".swift", ".kt", ".html", ".htm", ".css",
		".scss", ".less", ".toml", ".ini", ".conf", ".env",
	}, ext)
}

func isOfficeArchive(ext, mimeType string) bool {
	if slices.Contains([]string{".docx", ".xlsx", ".pptx", ".odt", ".ods", ".odp"}, ext) {
		return true
	}
	return strings.Contains(mimeType, "openxmlformats-officedocument") ||
		strings.Contains(mimeType, "opendocument")
}

func officeKind(ext, mimeType string) string {
	if ext != "" {
		return ext
	}
	switch {
	case strings.Contains(mimeType, "wordprocessingml"):
		return ".docx"
	case strings.Contains(mimeType, "spreadsheetml"):
		return ".xlsx"
	case strings.Contains(mimeType, "presentationml"):
		return ".pptx"
	case strings.Contains(mimeType, "opendocument.text"):
		return ".odt"
	case strings.Contains(mimeType, "opendocument.spreadsheet"):
		return ".ods"
	case strings.Contains(mimeType, "opendocument.presentation"):
		return ".odp"
	default:
		return ext
	}
}

func extractPlain(ctx context.Context, root string, file store.File) (extraction, error) {
	f, err := safefs.OpenRegular(root, file.StoragePath)
	if err != nil {
		return extraction{}, err
	}
	defer f.Close()
	buf := newCappedBuffer(maxExtractedBytes)
	if _, err := io.Copy(buf, io.LimitReader(contextReader{ctx: ctx, reader: f}, maxExtractedBytes+1)); err != nil {
		return extraction{}, err
	}
	return extraction{Text: normalizeText(buf.String()), Extractor: "plain-text", Truncated: buf.truncated}, nil
}

func extractOffice(ctx context.Context, root string, file store.File, ext string) (extraction, error) {
	f, err := safefs.OpenRegular(root, file.StoragePath)
	if err != nil {
		return extraction{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return extraction{}, err
	}
	if info.Size() > maxOfficeArchiveBytes {
		return extraction{}, fmt.Errorf("%w: Office 容器 %d 字节", ErrUnsafeInput, info.Size())
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return extraction{}, fmt.Errorf("读取 Office 容器: %w", err)
	}
	if len(zr.File) > maxOfficeArchiveEntries {
		return extraction{}, fmt.Errorf("%w: Office 容器条目超过 %d", ErrUnsafeInput, maxOfficeArchiveEntries)
	}
	entries := make([]*zip.File, 0, min(len(zr.File), maxOfficeEntries))
	var inflated uint64
	for _, entry := range zr.File {
		if officeTextEntry(ext, entry.Name) {
			if entry.UncompressedSize64 > maxOfficeEntryBytes {
				return extraction{}, fmt.Errorf("%w: %s 解压后 %d 字节", ErrUnsafeInput, entry.Name, entry.UncompressedSize64)
			}
			inflated += entry.UncompressedSize64
			if inflated > maxOfficeInflatedBytes {
				return extraction{}, fmt.Errorf("%w: Office 文本总量 %d 字节", ErrUnsafeInput, inflated)
			}
			if len(entries) == maxOfficeEntries {
				return extraction{}, fmt.Errorf("%w: Office 文本条目超过 %d", ErrUnsafeInput, maxOfficeEntries)
			}
			entries = append(entries, entry)
		}
	}
	slices.SortFunc(entries, func(a, b *zip.File) int { return strings.Compare(a.Name, b.Name) })
	buf := newCappedBuffer(maxExtractedBytes)
	remainingInflated := int64(maxOfficeInflatedBytes)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return extraction{}, err
		}
		if buf.truncated {
			break
		}
		r, err := entry.Open()
		if err != nil {
			return extraction{}, err
		}
		allowed := min(int64(maxOfficeEntryBytes), remainingInflated)
		counted := &countingReader{reader: io.LimitReader(contextReader{ctx: ctx, reader: r}, allowed+1)}
		_, decodeErr := copyXMLText(ctx, buf, counted)
		closeErr := r.Close()
		if counted.n > allowed {
			return extraction{}, fmt.Errorf("%w: Office 实际解压总量超过 %d 字节", ErrUnsafeInput, maxOfficeInflatedBytes)
		}
		remainingInflated -= counted.n
		if errors.Is(decodeErr, errExtractionLimit) {
			break
		}
		if decodeErr != nil {
			return extraction{}, fmt.Errorf("解析 %s: %w", entry.Name, decodeErr)
		}
		if closeErr != nil {
			return extraction{}, closeErr
		}
		_, _ = buf.Write([]byte("\n"))
	}
	return extraction{Text: normalizeText(buf.String()), Extractor: "office-xml", Truncated: buf.truncated}, nil
}

func officeTextEntry(ext, name string) bool {
	name = strings.ToLower(filepath.ToSlash(name))
	switch ext {
	case ".docx":
		return strings.HasPrefix(name, "word/") && strings.HasSuffix(name, ".xml")
	case ".xlsx":
		return name == "xl/sharedstrings.xml" || name == "xl/workbook.xml" ||
			(strings.HasPrefix(name, "xl/worksheets/") && strings.HasSuffix(name, ".xml"))
	case ".pptx":
		return (strings.HasPrefix(name, "ppt/slides/") || strings.HasPrefix(name, "ppt/notesslides/")) && strings.HasSuffix(name, ".xml")
	case ".odt", ".ods", ".odp":
		return name == "content.xml" || name == "styles.xml"
	default:
		return strings.HasSuffix(name, ".xml") && (strings.Contains(name, "document") || strings.Contains(name, "sharedstrings") || strings.Contains(name, "slides/") || name == "content.xml")
	}
}

func copyXMLText(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	decoder := xml.NewDecoder(src)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		data, ok := token.(xml.CharData)
		if !ok {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		n, err := io.WriteString(dst, text+"\n")
		written += int64(n)
		if err != nil {
			return written, err
		}
		if bounded, ok := dst.(*cappedBuffer); ok && bounded.truncated {
			return written, errExtractionLimit
		}
	}
}

func extractPDF(ctx context.Context, root string, file store.File) (extraction, error) {
	textLayer, textErr := extractCommand(ctx, root, file, "pdftotext", []string{"-layout", "{file}", "-"}, "pdftotext", maxPDFInputBytes)
	if textErr == nil && strings.TrimSpace(textLayer.Text) != "" {
		return textLayer, nil
	}
	ocr, ocrErr := extractPDFOCR(ctx, root, file)
	if ocrErr == nil {
		return ocr, nil
	}
	if textErr == nil && errors.Is(ocrErr, ErrUnsupported) {
		return extraction{}, fmt.Errorf("%w: PDF 没有文本层且 OCR 不可用", ErrUnsupported)
	}
	if textErr == nil {
		return extraction{}, ocrErr
	}
	return extraction{}, errors.Join(textErr, ocrErr)
}

func extractPDFOCR(ctx context.Context, root string, file store.File) (extraction, error) {
	for _, command := range []string{"pdftoppm", "tesseract"} {
		if _, err := exec.LookPath(command); err != nil {
			return extraction{}, fmt.Errorf("%w: %s 未安装", ErrUnsupported, command)
		}
	}
	path, cleanup, err := copyRegularToTemp(ctx, root, file, maxPDFInputBytes)
	if err != nil {
		return extraction{}, err
	}
	defer cleanup()
	dir, err := os.MkdirTemp("", "nbco-pdf-ocr-*")
	if err != nil {
		return extraction{}, err
	}
	defer os.RemoveAll(dir)
	prefix := filepath.Join(dir, "page")
	stderr := newCappedBuffer(32 << 10)
	cmd := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", "150", "-f", "1", "-l", strconv.Itoa(maxOCRPages), path, prefix)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return extraction{}, ctx.Err()
		}
		return extraction{}, fmt.Errorf("PDF 转图失败: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	images, err := filepath.Glob(prefix + "-*.png")
	if err != nil {
		return extraction{}, err
	}
	sort.SliceStable(images, func(i, j int) bool {
		left, leftOK := pdfPageNumber(images[i])
		right, rightOK := pdfPageNumber(images[j])
		if leftOK && rightOK && left != right {
			return left < right
		}
		return images[i] < images[j]
	})
	if len(images) == 0 {
		return extraction{}, errors.New("PDF OCR 未生成页面图像")
	}
	var imageBytes int64
	for _, image := range images {
		info, statErr := os.Stat(image)
		if statErr != nil {
			return extraction{}, statErr
		}
		if info.Size() > maxOCRImageBytes {
			return extraction{}, fmt.Errorf("%w: OCR 页面 %s 为 %d 字节", ErrUnsafeInput, filepath.Base(image), info.Size())
		}
		imageBytes += info.Size()
		if imageBytes > maxOCRTotalBytes {
			return extraction{}, fmt.Errorf("%w: OCR 页面总量 %d 字节", ErrUnsafeInput, imageBytes)
		}
	}
	buf := newCappedBuffer(maxExtractedBytes)
	language := preferredTesseractLanguage(ctx)
	for _, image := range images {
		if err := ctx.Err(); err != nil {
			return extraction{}, err
		}
		args := []string{image, "stdout"}
		if language != "" {
			args = append(args, "-l", language)
		}
		stderr := newCappedBuffer(16 << 10)
		cmd := exec.CommandContext(ctx, "tesseract", args...)
		cmd.Stdout = buf
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return extraction{}, ctx.Err()
			}
			return extraction{}, fmt.Errorf("PDF OCR 失败: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		_, _ = buf.Write([]byte("\n"))
		if buf.truncated {
			break
		}
	}
	return extraction{
		Text:      normalizeText(buf.String()),
		Extractor: "pdf-ocr",
		Truncated: buf.truncated || len(images) >= maxOCRPages,
	}, nil
}

func extractCommand(ctx context.Context, root string, file store.File, command string, args []string, extractor string, maxInputBytes int64) (extraction, error) {
	if _, err := exec.LookPath(command); err != nil {
		return extraction{}, fmt.Errorf("%w: %s 未安装", ErrUnsupported, command)
	}
	path, cleanup, err := copyRegularToTemp(ctx, root, file, maxInputBytes)
	if err != nil {
		return extraction{}, err
	}
	defer cleanup()
	actualArgs := make([]string, len(args))
	for i, arg := range args {
		actualArgs[i] = strings.ReplaceAll(arg, "{file}", path)
	}
	cmd := exec.CommandContext(ctx, command, actualArgs...)
	stdout := newCappedBuffer(maxExtractedBytes)
	stderr := newCappedBuffer(16 << 10)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return extraction{}, ctx.Err()
		}
		return extraction{}, fmt.Errorf("%s 提取失败: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return extraction{Text: normalizeText(stdout.String()), Extractor: extractor, Truncated: stdout.truncated}, nil
}

func copyRegularToTemp(ctx context.Context, root string, file store.File, maxInputBytes int64) (string, func(), error) {
	if maxInputBytes > 0 && file.SizeBytes > maxInputBytes {
		return "", func() {}, fmt.Errorf("%w: 输入文件声明大小 %d 字节", ErrUnsafeInput, file.SizeBytes)
	}
	f, err := safefs.OpenRegular(root, file.StoragePath)
	if err != nil {
		return "", func() {}, err
	}
	defer f.Close()
	if maxInputBytes > 0 {
		info, statErr := f.Stat()
		if statErr != nil {
			return "", func() {}, statErr
		}
		if info.Size() > maxInputBytes {
			return "", func() {}, fmt.Errorf("%w: 输入文件实际大小 %d 字节", ErrUnsafeInput, info.Size())
		}
	}
	tmp, err := os.CreateTemp("", "nbco-index-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	input := io.Reader(contextReader{ctx: ctx, reader: f})
	if maxInputBytes > 0 {
		input = io.LimitReader(input, maxInputBytes+1)
	}
	written, err := io.Copy(tmp, input)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if maxInputBytes > 0 && written > maxInputBytes {
		cleanup()
		return "", func() {}, fmt.Errorf("%w: 输入文件实际大小超过 %d 字节", ErrUnsafeInput, maxInputBytes)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmp.Name(), cleanup, nil
}

func pdfPageNumber(path string) (int, bool) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	separator := strings.LastIndexByte(base, '-')
	if separator < 0 || separator == len(base)-1 {
		return 0, false
	}
	page, err := strconv.Atoi(base[separator+1:])
	return page, err == nil && page > 0
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

type countingReader struct {
	reader io.Reader
	n      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.n += int64(n)
	return n, err
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err == nil {
		if contextErr := r.ctx.Err(); contextErr != nil {
			return n, contextErr
		}
	}
	return n, err
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer { return &cappedBuffer{limit: max(0, limit)} }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return original, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

func normalizeText(text string) string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, " ")
	}
	text = strings.ReplaceAll(text, "\x00", " ")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func splitText(text string) []string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}
	chunks := make([]string, 0, (len(runes)+maxChunkRunes-1)/maxChunkRunes)
	for start := 0; start < len(runes); {
		end := min(start+maxChunkRunes, len(runes))
		if end < len(runes) {
			floor := start + maxChunkRunes/2
			for i := end - 1; i >= floor; i-- {
				if strings.ContainsRune("\n。！？!?；;.", runes[i]) {
					end = i + 1
					break
				}
			}
		}
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		if end == len(runes) {
			break
		}
		next := max(start+1, end-chunkOverlapRunes)
		start = next
	}
	return chunks
}
