package ai

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ledongthuc/pdf"
	"github.com/ulikunitz/xz"
)

const (
	ZIMMagic        = 0x044d495a
	maxZimArticles  = 120000
	maxDocTextRunes = 32000
	minArticleChars = 60
	maxClusterBytes = 64 * 1024 * 1024
)

func htmlToPlainText(raw, fallbackTitle string, maxRunes int) (title, text string) {
	title = fallbackTitle
	if m := titleTagRe.FindStringSubmatch(raw); len(m) > 1 {
		t := strings.TrimSpace(html.UnescapeString(htmlTagRe.ReplaceAllString(m[1], "")))
		if t != "" {
			title = t
		}
	}
	cleaned := scriptStyleRe.ReplaceAllString(raw, " ")
	cleaned = htmlTagRe.ReplaceAllString(cleaned, "\n")
	cleaned = html.UnescapeString(cleaned)
	cleaned = multiSpaceRe.ReplaceAllString(cleaned, " ")
	cleaned = multiNewline.ReplaceAllString(cleaned, "\n\n")
	cleaned = strings.TrimSpace(cleaned)

	if maxRunes > 0 {
		runes := []rune(cleaned)
		if len(runes) > maxRunes {
			cleaned = string(runes[:maxRunes])
		}
	}
	return title, cleaned
}

// ZIMHeader holds fixed-width OpenZIM archive metadata.
type ZIMHeader struct {
	MajorVersion  uint16
	MinorVersion  uint16
	ArticleCount  uint32
	ClusterCount  uint32
	URLPtrPos     uint64
	TitlePtrPos   uint64
	ClusterPtrPos uint64
	MimeListPos   uint64
	MainPage      uint32
	ChecksumPos   uint64
}

type zimEntry struct {
	URL     string
	Title   string
	Cluster uint32
	Blob    uint32
}

// ReadZIMHeader decodes the 80-byte OpenZIM file header.
func ReadZIMHeader(r io.ReadSeeker) (ZIMHeader, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return ZIMHeader{}, err
	}
	var buf [80]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return ZIMHeader{}, fmt.Errorf("read zim header: %w", err)
	}
	magic := binary.LittleEndian.Uint32(buf[0:4])
	if magic != ZIMMagic {
		return ZIMHeader{}, fmt.Errorf("invalid zim magic: 0x%x", magic)
	}
	return ZIMHeader{
		MajorVersion:  binary.LittleEndian.Uint16(buf[4:6]),
		MinorVersion:  binary.LittleEndian.Uint16(buf[6:8]),
		ArticleCount:  binary.LittleEndian.Uint32(buf[24:28]),
		ClusterCount:  binary.LittleEndian.Uint32(buf[28:32]),
		URLPtrPos:     binary.LittleEndian.Uint64(buf[32:40]),
		TitlePtrPos:   binary.LittleEndian.Uint64(buf[40:48]),
		ClusterPtrPos: binary.LittleEndian.Uint64(buf[48:56]),
		MimeListPos:   binary.LittleEndian.Uint64(buf[56:64]),
		MainPage:      binary.LittleEndian.Uint32(buf[64:68]),
		ChecksumPos:   binary.LittleEndian.Uint64(buf[72:80]),
	}, nil
}

func readZIMMimeTypes(r io.ReadSeeker, pos uint64) (map[uint16]bool, error) {
	if _, err := r.Seek(int64(pos), io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(r, 8192)
	htmlMimes := make(map[uint16]bool)
	for id := uint16(0); id < 1024; id++ {
		s, err := br.ReadString(0)
		if err != nil {
			return nil, fmt.Errorf("read zim mime list: %w", err)
		}
		mt := strings.ToLower(strings.TrimSuffix(s, "\x00"))
		if mt == "" {
			break
		}
		if strings.HasPrefix(mt, "text/html") || strings.HasPrefix(mt, "text/plain") {
			htmlMimes[id] = true
		}
	}
	return htmlMimes, nil
}

func readNullString(br *bufio.Reader) (string, error) {
	s, err := br.ReadString(0)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s, "\x00"), nil
}

// extractZIM reads text/html articles directly from a ZIM archive without Docker.
// Entries are grouped by cluster so each cluster is decompressed at most once.
func extractZIM(ctx context.Context, filePath string) ([]ExtractedDoc, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	hdr, err := ReadZIMHeader(f)
	if err != nil {
		return nil, err
	}
	if hdr.ArticleCount == 0 || hdr.ClusterCount == 0 {
		return nil, fmt.Errorf("zim archive contains no articles")
	}

	htmlMimes, err := readZIMMimeTypes(f, hdr.MimeListPos)
	if err != nil {
		return nil, err
	}
	if len(htmlMimes) == 0 {
		return nil, fmt.Errorf("no HTML or text mime types found in %s", filepath.Base(filePath))
	}

	urlPtrs := make([]uint64, hdr.ArticleCount)
	if _, err := f.Seek(int64(hdr.URLPtrPos), io.SeekStart); err != nil {
		return nil, err
	}
	if err := binary.Read(bufio.NewReaderSize(f, 64*1024), binary.LittleEndian, urlPtrs); err != nil {
		return nil, fmt.Errorf("read zim url pointers: %w", err)
	}

	clusterPtrs := make([]uint64, hdr.ClusterCount)
	if _, err := f.Seek(int64(hdr.ClusterPtrPos), io.SeekStart); err != nil {
		return nil, err
	}
	if err := binary.Read(bufio.NewReaderSize(f, 64*1024), binary.LittleEndian, clusterPtrs); err != nil {
		return nil, fmt.Errorf("read zim cluster pointers: %w", err)
	}

	// Sort directory entry offsets ascending so scanning mostly stays sequential in bufio without 800k syscalls.
	slices.Sort(urlPtrs)

	candidates := make([]zimEntry, 0, min(int(hdr.ArticleCount), 32768))
	var fixedBuf [16]byte
	var curPos uint64 = ^uint64(0)
	br := bufio.NewReaderSize(f, 256*1024)

	for i, ptr := range urlPtrs {
		if i%4000 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if curPos != ptr {
			if ptr > curPos && ptr-curPos <= 64*1024 {
				n, derr := br.Discard(int(ptr - curPos))
				curPos += uint64(n)
				if derr != nil || curPos != ptr {
					curPos = ^uint64(0)
				}
			}
			if curPos != ptr {
				if _, err := f.Seek(int64(ptr), io.SeekStart); err != nil {
					curPos = ^uint64(0)
					continue
				}
				br.Reset(f)
				curPos = ptr
			}
		}

		// Directory entry header: mimeType(2) + paramLen(1) + ns(1) + rev(4) + cluster(4) + blob(4)
		if _, err := io.ReadFull(br, fixedBuf[:2]); err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += 2
		mimeID := binary.LittleEndian.Uint16(fixedBuf[0:2])
		// Redirect (0xffff), linktarget (0xfffe), deleted (0xfffd)
		if mimeID >= 0xfffd || !htmlMimes[mimeID] {
			continue
		}
		if _, err := io.ReadFull(br, fixedBuf[2:16]); err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += 14
		ns := fixedBuf[3]
		if (hdr.MajorVersion >= 6 && ns != 'C') || (hdr.MajorVersion < 6 && ns != 'A') {
			continue
		}
		clusterNum := binary.LittleEndian.Uint32(fixedBuf[8:12])
		blobNum := binary.LittleEndian.Uint32(fixedBuf[12:16])
		if clusterNum >= hdr.ClusterCount {
			continue
		}
		urlStr, err := readNullString(br)
		if err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += uint64(len(urlStr) + 1)
		titleStr, err := readNullString(br)
		if err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += uint64(len(titleStr) + 1)
		if titleStr == "" {
			titleStr = urlStr
		}
		candidates = append(candidates, zimEntry{
			URL:     urlStr,
			Title:   titleStr,
			Cluster: clusterNum,
			Blob:    blobNum,
		})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no extractable HTML articles found in %s", filepath.Base(filePath))
	}

	// If archive exceeds maxZimArticles, sample evenly across all titles/clusters rather than stopping early.
	byCluster := make(map[uint32][]zimEntry)
	if len(candidates) <= maxZimArticles {
		for _, entry := range candidates {
			byCluster[entry.Cluster] = append(byCluster[entry.Cluster], entry)
		}
	} else {
		for idx := 0; idx < maxZimArticles; idx++ {
			cIdx := (idx * len(candidates)) / maxZimArticles
			entry := candidates[cIdx]
			byCluster[entry.Cluster] = append(byCluster[entry.Cluster], entry)
		}
	}

	clusterIDs := make([]uint32, 0, len(byCluster))
	for cid := range byCluster {
		clusterIDs = append(clusterIDs, cid)
	}
	sort.Slice(clusterIDs, func(i, j int) bool { return clusterIDs[i] < clusterIDs[j] })

	dataEnd := uint64(stat.Size())
	if hdr.ChecksumPos > hdr.ClusterPtrPos && hdr.ChecksumPos < dataEnd {
		dataEnd = hdr.ChecksumPos
	}

	zstdDec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer zstdDec.Close()

	docs := make([]ExtractedDoc, 0, min(len(candidates), maxZimArticles))
	for _, cid := range clusterIDs {
		if ctx.Err() != nil {
			return docs, ctx.Err()
		}
		startPos := clusterPtrs[cid]
		endPos := dataEnd
		if cid+1 < hdr.ClusterCount {
			next := clusterPtrs[cid+1]
			if next > startPos {
				endPos = next
			}
		}
		// Keep within cluster data bounds
		if endPos <= startPos || endPos-startPos > maxClusterBytes {
			continue
		}
		rawCluster := make([]byte, endPos-startPos)
		if _, err := f.Seek(int64(startPos), io.SeekStart); err != nil {
			continue
		}
		if _, err := io.ReadFull(f, rawCluster); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			continue
		}

		blobs, err := ParseZIMCluster(rawCluster, zstdDec)
		if err != nil || len(blobs) == 0 {
			continue
		}
		for _, entry := range byCluster[cid] {
			if int(entry.Blob) >= len(blobs) {
				continue
			}
			_, plain := htmlToPlainText(string(blobs[entry.Blob]), entry.Title, maxDocTextRunes)
			if len([]rune(plain)) < minArticleChars {
				continue
			}
			docs = append(docs, ExtractedDoc{
				Path:  filePath + "#" + entry.URL,
				Title: entry.Title,
				Text:  plain,
			})
		}
	}

	if len(docs) == 0 {
		return nil, fmt.Errorf("no readable text extracted from %s", filepath.Base(filePath))
	}
	return docs, nil
}

// ParseZIMCluster decompresses a single OpenZIM cluster and returns its blob slices.
func ParseZIMCluster(raw []byte, zstdDec *zstd.Decoder) ([][]byte, error) {
	if len(raw) < 5 {
		return nil, fmt.Errorf("cluster too short")
	}
	info := raw[0]
	compType := info & 0x0f
	extended := (info & 0x10) != 0
	payload := raw[1:]

	var data []byte
	switch compType {
	case 0, 1:
		data = payload
	case 4: // LZMA2 / XZ
		xr, err := xz.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		decompressed, err := io.ReadAll(io.LimitReader(xr, maxClusterBytes))
		if err != nil {
			return nil, err
		}
		data = decompressed
	case 5: // Zstandard
		decompressed, err := zstdDec.DecodeAll(payload, nil)
		if err != nil {
			return nil, err
		}
		data = decompressed
	default:
		return nil, fmt.Errorf("unsupported zim cluster compression %d", compType)
	}

	offSize := 4
	if extended {
		offSize = 8
	}
	if len(data) < offSize*2 {
		return nil, fmt.Errorf("cluster offset table too small")
	}
	readOff := func(idx int) int {
		pos := idx * offSize
		if pos+offSize > len(data) {
			return -1
		}
		if extended {
			v := binary.LittleEndian.Uint64(data[pos : pos+8])
			if v > uint64(len(data)) {
				return -1
			}
			return int(v)
		}
		v := binary.LittleEndian.Uint32(data[pos : pos+4])
		if int(v) > len(data) {
			return -1
		}
		return int(v)
	}

	firstOff := readOff(0)
	if firstOff < offSize*2 || firstOff%offSize != 0 || firstOff > len(data) {
		return nil, fmt.Errorf("invalid first blob offset %d", firstOff)
	}
	numOffsets := firstOff / offSize
	if numOffsets < 2 {
		return nil, nil
	}
	blobs := make([][]byte, 0, numOffsets-1)
	for i := 0; i < numOffsets-1; i++ {
		s := readOff(i)
		e := readOff(i + 1)
		if s < 0 || e < s || e > len(data) {
			blobs = append(blobs, nil)
			continue
		}
		blobs = append(blobs, data[s:e])
	}
	return blobs, nil
}

// extractEPUB extracts text directly from HTML/XHTML chapters inside an EPUB ZIP archive.
func extractEPUB(ctx context.Context, filePath string) ([]ExtractedDoc, error) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return nil, fmt.Errorf("open epub: %w", err)
	}
	defer zr.Close()

	var docs []ExtractedDoc
	for _, zf := range zr.File {
		if ctx.Err() != nil {
			return docs, ctx.Err()
		}
		lower := strings.ToLower(zf.Name)
		if !strings.HasSuffix(lower, ".html") && !strings.HasSuffix(lower, ".xhtml") && !strings.HasSuffix(lower, ".htm") {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(rc, 2*1024*1024))
		rc.Close()
		if readErr != nil {
			continue
		}
		title, plain := htmlToPlainText(string(b), filepath.Base(zf.Name), maxDocTextRunes)
		if len([]rune(plain)) < minArticleChars {
			continue
		}
		docs = append(docs, ExtractedDoc{
			Path:  filePath + "#" + zf.Name,
			Title: title,
			Text:  plain,
		})
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("no readable text found in %s", filepath.Base(filePath))
	}
	return docs, nil
}

// extractPDF extracts plain text pages natively via github.com/ledongthuc/pdf.
func extractPDF(ctx context.Context, filePath string) (docs []ExtractedDoc, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("pdf parser error for %s: %v", filepath.Base(filePath), rec)
		}
	}()

	f, r, openErr := pdf.Open(filePath)
	if openErr != nil {
		return nil, fmt.Errorf("open pdf: %w", openErr)
	}
	defer f.Close()

	numPages := r.NumPage()
	if numPages == 0 {
		return nil, fmt.Errorf("pdf contains no pages: %s", filepath.Base(filePath))
	}

	var b strings.Builder
	for p := 1; p <= numPages; p++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		page := r.Page(p)
		if page.V.IsNull() {
			continue
		}
		txt, perr := page.GetPlainText(nil)
		if perr != nil || strings.TrimSpace(txt) == "" {
			continue
		}
		b.WriteString(strings.TrimSpace(txt))
		b.WriteString("\n\n")
	}

	fullText := strings.TrimSpace(b.String())
	if fullText == "" {
		return nil, fmt.Errorf("no selectable text found in %s (scanned image PDFs require OCR)", filepath.Base(filePath))
	}
	return []ExtractedDoc{{
		Path:  filePath,
		Title: strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath)),
		Text:  fullText,
	}}, nil
}
