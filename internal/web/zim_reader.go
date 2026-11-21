package web

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ark/internal/ai"

	"github.com/klauspost/compress/zstd"
)

type cachedZIMCluster struct {
	id    uint32
	blobs [][]byte
}

type zimTitleRecord struct {
	ns    byte
	url   string
	title string
}

type zimArchive struct {
	mu          sync.Mutex
	path        string
	stem        string
	size        int64
	modUnixNano int64
	hdr         ai.ZIMHeader
	mimes       []string
	dataEnd     uint64
	zstdDec     *zstd.Decoder
	clusters    []cachedZIMCluster
	titles      []zimTitleRecord
	titlesBuilt bool
	metaLoaded  bool
	metaTitle   string
	metaDesc    string
}

var (
	zimCacheMu sync.Mutex
	zimCache   = make(map[string]*zimArchive)
)

func getZIMArchive(filePath, stem string) (*zimArchive, error) {
	st, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}

	zimCacheMu.Lock()
	existing := zimCache[filePath]
	if existing != nil && existing.size == st.Size() && existing.modUnixNano == st.ModTime().UnixNano() {
		zimCacheMu.Unlock()
		return existing, nil
	}
	if existing != nil {
		existing.close()
		delete(zimCache, filePath)
	}
	zimCacheMu.Unlock()

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr, err := ai.ReadZIMHeader(f)
	if err != nil {
		return nil, err
	}

	if _, err := f.Seek(int64(hdr.MimeListPos), io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 8192)
	var mimes []string
	for len(mimes) < 1024 {
		s, err := br.ReadString(0)
		if err != nil {
			return nil, fmt.Errorf("read zim mimes: %w", err)
		}
		mt := strings.TrimSuffix(s, "\x00")
		if mt == "" {
			break
		}
		mimes = append(mimes, mt)
	}

	zdec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}

	dataEnd := uint64(st.Size())
	if hdr.ChecksumPos > hdr.ClusterPtrPos && hdr.ChecksumPos < dataEnd {
		dataEnd = hdr.ChecksumPos
	}

	arc := &zimArchive{
		path:        filePath,
		stem:        stem,
		size:        st.Size(),
		modUnixNano: st.ModTime().UnixNano(),
		hdr:         hdr,
		mimes:       mimes,
		dataEnd:     dataEnd,
		zstdDec:     zdec,
	}

	zimCacheMu.Lock()
	zimCache[filePath] = arc
	zimCacheMu.Unlock()
	return arc, nil
}

func (a *zimArchive) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.zstdDec != nil {
		a.zstdDec.Close()
		a.zstdDec = nil
	}
}

type zimResolvedEntry struct {
	mimeID      uint16
	ns          byte
	clusterNum  uint32
	blobNum     uint32
	redirectIdx uint32
	url         string
	title       string
}

func (a *zimArchive) readEntryAt(f *os.File, idx uint32) (zimResolvedEntry, error) {
	if idx >= a.hdr.ArticleCount {
		return zimResolvedEntry{}, fmt.Errorf("entry index out of range")
	}
	var ptrBuf [8]byte
	if _, err := f.ReadAt(ptrBuf[:], int64(a.hdr.URLPtrPos)+int64(idx)*8); err != nil {
		return zimResolvedEntry{}, err
	}
	pos := binary.LittleEndian.Uint64(ptrBuf[:])
	return a.readEntryAtOffset(f, pos)
}

func (a *zimArchive) readEntryAtOffset(f *os.File, pos uint64) (zimResolvedEntry, error) {
	var buf [512]byte
	n, err := f.ReadAt(buf[:], int64(pos))
	if err != nil && !errors.Is(err, io.EOF) && n < 12 {
		return zimResolvedEntry{}, err
	}
	raw := buf[:n]
	if len(raw) < 12 {
		return zimResolvedEntry{}, fmt.Errorf("short entry")
	}
	mimeID := binary.LittleEndian.Uint16(raw[0:2])
	ns := raw[3]
	var e zimResolvedEntry
	e.mimeID = mimeID
	e.ns = ns

	var strStart int
	if mimeID == 0xffff {
		e.redirectIdx = binary.LittleEndian.Uint32(raw[8:12])
		strStart = 12
	} else if mimeID >= 0xfffd {
		strStart = 8
	} else {
		if len(raw) < 16 {
			return zimResolvedEntry{}, fmt.Errorf("short content entry")
		}
		e.clusterNum = binary.LittleEndian.Uint32(raw[8:12])
		e.blobNum = binary.LittleEndian.Uint32(raw[12:16])
		strStart = 16
	}

	if strStart < len(raw) {
		rest := raw[strStart:]
		if z1 := bytes.IndexByte(rest, 0); z1 >= 0 {
			e.url = string(rest[:z1])
			rest2 := rest[z1+1:]
			if z2 := bytes.IndexByte(rest2, 0); z2 >= 0 {
				e.title = string(rest2[:z2])
			} else {
				e.title = string(rest2)
			}
		}
	}
	if e.title == "" {
		e.title = e.url
	}
	return e, nil
}

func (a *zimArchive) findEntry(f *os.File, ns byte, urlPath string) (zimResolvedEntry, bool) {
	_, e, ok := a.findEntryWithIndex(f, ns, urlPath)
	return e, ok
}

func (a *zimArchive) findEntryWithIndex(f *os.File, ns byte, urlPath string) (int, zimResolvedEntry, bool) {
	if a.hdr.ArticleCount == 0 {
		return -1, zimResolvedEntry{}, false
	}
	n := int(a.hdr.ArticleCount)
	idx := sort.Search(n, func(i int) bool {
		e, err := a.readEntryAt(f, uint32(i))
		if err != nil {
			return false
		}
		if e.ns != ns {
			return e.ns > ns
		}
		return e.url >= urlPath
	})
	if idx >= n {
		return -1, zimResolvedEntry{}, false
	}
	e, err := a.readEntryAt(f, uint32(idx))
	if err != nil || e.ns != ns || e.url != urlPath {
		return -1, zimResolvedEntry{}, false
	}
	return idx, e, true
}

func (a *zimArchive) adjacentHTMLPages(f *os.File, idx int, ns byte) (prev *zimTitleRecord, next *zimTitleRecord) {
	if idx < 0 || uint32(idx) >= a.hdr.ArticleCount {
		return nil, nil
	}
	const maxScan = 25000
	for i := idx - 1; i >= 0 && idx-i <= maxScan; i-- {
		e, err := a.readEntryAt(f, uint32(i))
		if err != nil {
			continue
		}
		if e.ns != ns {
			break
		}
		if e.mimeID < 0xfffd && int(e.mimeID) < len(a.mimes) && strings.HasPrefix(strings.ToLower(a.mimes[e.mimeID]), "text/html") {
			prev = &zimTitleRecord{ns: e.ns, url: e.url, title: e.title}
			break
		}
	}
	n := int(a.hdr.ArticleCount)
	for i := idx + 1; i < n && i-idx <= maxScan; i++ {
		e, err := a.readEntryAt(f, uint32(i))
		if err != nil {
			continue
		}
		if e.ns != ns {
			break
		}
		if e.mimeID < 0xfffd && int(e.mimeID) < len(a.mimes) && strings.HasPrefix(strings.ToLower(a.mimes[e.mimeID]), "text/html") {
			next = &zimTitleRecord{ns: e.ns, url: e.url, title: e.title}
			break
		}
	}
	return prev, next
}

func (a *zimArchive) followRedirects(f *os.File, e zimResolvedEntry) (zimResolvedEntry, error) {
	for hop := 0; hop < 8 && e.mimeID == 0xffff; hop++ {
		next, err := a.readEntryAt(f, e.redirectIdx)
		if err != nil {
			return e, err
		}
		e = next
	}
	return e, nil
}

func (a *zimArchive) readBlob(f *os.File, clusterNum, blobNum uint32) ([]byte, error) {
	for i := range a.clusters {
		if a.clusters[i].id == clusterNum {
			blobs := a.clusters[i].blobs
			if int(blobNum) >= len(blobs) {
				return nil, fmt.Errorf("blob index %d out of range", blobNum)
			}
			return blobs[blobNum], nil
		}
	}
	if clusterNum >= a.hdr.ClusterCount {
		return nil, fmt.Errorf("cluster %d out of range", clusterNum)
	}

	var pair [16]byte
	if _, err := f.ReadAt(pair[:8], int64(a.hdr.ClusterPtrPos)+int64(clusterNum)*8); err != nil {
		return nil, err
	}
	startPos := binary.LittleEndian.Uint64(pair[:8])
	endPos := a.dataEnd
	if clusterNum+1 < a.hdr.ClusterCount {
		if _, err := f.ReadAt(pair[8:16], int64(a.hdr.ClusterPtrPos)+int64(clusterNum+1)*8); err == nil {
			next := binary.LittleEndian.Uint64(pair[8:16])
			if next > startPos {
				endPos = next
			}
		}
	}
	const maxRead = 64 * 1024 * 1024
	if endPos <= startPos || endPos-startPos > maxRead {
		return nil, fmt.Errorf("invalid cluster bounds")
	}

	raw := make([]byte, endPos-startPos)
	if _, err := f.ReadAt(raw, int64(startPos)); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	blobs, err := ai.ParseZIMCluster(raw, a.zstdDec)
	if err != nil {
		return nil, err
	}

	if len(a.clusters) >= 8 {
		a.clusters = append(a.clusters[1:], cachedZIMCluster{id: clusterNum, blobs: blobs})
	} else {
		a.clusters = append(a.clusters, cachedZIMCluster{id: clusterNum, blobs: blobs})
	}

	if int(blobNum) >= len(blobs) {
		return nil, fmt.Errorf("blob index %d out of range", blobNum)
	}
	return blobs[blobNum], nil
}

func (a *zimArchive) readTitleEntryIdx(f *os.File, titlePosIdx uint32) (uint32, error) {
	if titlePosIdx >= a.hdr.ArticleCount || a.hdr.TitlePtrPos == 0 {
		return 0, fmt.Errorf("title index out of range")
	}
	var buf [4]byte
	if _, err := f.ReadAt(buf[:], int64(a.hdr.TitlePtrPos)+int64(titlePosIdx)*4); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

func (a *zimArchive) readMetaEntryBytes(f *os.File, key string) ([]byte, string) {
	e, ok := a.findEntry(f, 'M', key)
	if !ok {
		return nil, ""
	}
	if e.mimeID == 0xffff {
		t, rerr := a.followRedirects(f, e)
		if rerr != nil {
			return nil, ""
		}
		e = t
	}
	if e.mimeID >= 0xfffd {
		return nil, ""
	}
	blob, err := a.readBlob(f, e.clusterNum, e.blobNum)
	if err != nil || len(blob) == 0 {
		return nil, ""
	}
	mt := ""
	if int(e.mimeID) < len(a.mimes) {
		mt = a.mimes[e.mimeID]
	}
	return blob, mt
}

func (a *zimArchive) metadata() (title, desc string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.metaLoaded {
		return a.metaTitle, a.metaDesc
	}
	a.metaLoaded = true
	if f, err := os.Open(a.path); err == nil {
		defer f.Close()
		if raw, _ := a.readMetaEntryBytes(f, "Title"); len(raw) > 0 {
			a.metaTitle = strings.TrimSpace(string(raw))
		}
		if raw, _ := a.readMetaEntryBytes(f, "Description"); len(raw) > 0 {
			a.metaDesc = strings.TrimSpace(string(raw))
		}
	}
	return a.metaTitle, a.metaDesc
}

func (a *zimArchive) embeddedIcon() ([]byte, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.Open(a.path)
	if err != nil {
		return nil, ""
	}
	defer f.Close()
	for _, key := range []string{"Illustration_48x48@1", "Illustration_96x96@1", "favicon"} {
		if raw, mt := a.readMetaEntryBytes(f, key); len(raw) > 0 {
			if mt == "" {
				mt = http.DetectContentType(raw)
			}
			return raw, mt
		}
	}
	return nil, ""
}

func (a *zimArchive) hasValidTitlePtr() bool {
	return a.hdr.TitlePtrPos > 0 && a.hdr.TitlePtrPos != ^uint64(0) && a.hdr.TitlePtrPos+uint64(a.hdr.ArticleCount)*4 <= uint64(a.size)
}

func isLikelyAssetURL(u string) bool {
	if u == "" {
		return true
	}
	if strings.HasPrefix(u, "-/") || strings.HasPrefix(u, "I/") || strings.HasPrefix(u, "M/") || strings.HasPrefix(u, "X/") || strings.HasPrefix(u, "_mw_") {
		return true
	}
	low := strings.ToLower(u)
	for _, ext := range []string{".css", ".js", ".mjs", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico", ".woff", ".woff2", ".ttf", ".otf", ".eot", ".json", ".map", ".mp3", ".mp4", ".webm", ".ogg", ".wav", ".pdf", ".xml"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

func (a *zimArchive) isSearchableArticle(ns, wantNS byte, mimeID uint16, urlStr string) bool {
	if ns != wantNS {
		return false
	}
	if mimeID == 0xffff {
		return !isLikelyAssetURL(urlStr)
	}
	if mimeID >= 0xfffd || int(mimeID) >= len(a.mimes) {
		return false
	}
	return strings.HasPrefix(strings.ToLower(a.mimes[mimeID]), "text/html")
}

func (a *zimArchive) buildSmallTitleIndexLocked() {
	if a.titlesBuilt || a.hdr.ArticleCount == 0 || a.hdr.ArticleCount > 40000 {
		a.titlesBuilt = true
		return
	}
	wantNS := byte('C')
	if a.hdr.MajorVersion < 6 {
		wantNS = 'A'
	}
	if f, err := os.Open(a.path); err == nil {
		urlPtrs := make([]uint64, a.hdr.ArticleCount)
		if _, err := f.Seek(int64(a.hdr.URLPtrPos), io.SeekStart); err == nil {
			if err := binary.Read(bufio.NewReaderSize(f, 64*1024), binary.LittleEndian, urlPtrs); err == nil {
				sort.Slice(urlPtrs, func(i, j int) bool { return urlPtrs[i] < urlPtrs[j] })
				br := bufio.NewReaderSize(f, 256*1024)
				var curPos uint64 = ^uint64(0)
				var fixed [16]byte
				for _, ptr := range urlPtrs {
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
					if _, err := io.ReadFull(br, fixed[:2]); err != nil {
						curPos = ^uint64(0)
						continue
					}
					curPos += 2
					mimeID := binary.LittleEndian.Uint16(fixed[:2])
					if mimeID == 0xfffd || mimeID == 0xfffe {
						continue
					}
					hdrRest := 14
					if mimeID == 0xffff {
						hdrRest = 10
					}
					if _, err := io.ReadFull(br, fixed[2:2+hdrRest]); err != nil {
						curPos = ^uint64(0)
						continue
					}
					curPos += uint64(hdrRest)
					ns := fixed[3]
					uStr, err := br.ReadString(0)
					if err != nil {
						curPos = ^uint64(0)
						continue
					}
					curPos += uint64(len(uStr))
					uStr = strings.TrimSuffix(uStr, "\x00")
					tStr, err := br.ReadString(0)
					if err != nil {
						curPos = ^uint64(0)
						continue
					}
					curPos += uint64(len(tStr))
					tStr = strings.TrimSuffix(tStr, "\x00")
					if tStr == "" {
						tStr = uStr
					}
					if a.isSearchableArticle(ns, wantNS, mimeID, uStr) {
						a.titles = append(a.titles, zimTitleRecord{ns: ns, url: uStr, title: tStr})
					}
				}
			}
		}
		_ = f.Close()
	}
	a.titlesBuilt = true
}

func (a *zimArchive) searchTitles(query string, limit int) []zimTitleRecord {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	terms := strings.Fields(q)
	if len(terms) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 40
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.hdr.ArticleCount > 40000 {
		if a.hasValidTitlePtr() {
			return a.searchLargeZIMByTitlePtr(q, terms, limit)
		}
		return a.scanTitlesStreaming(q, terms, limit)
	}

	a.buildSmallTitleIndexLocked()
	return rankZIMTitleMatches(a.titles, q, terms, limit)
}

func isWordBoundaryPrefix(low, q string) bool {
	if !strings.HasPrefix(low, q) {
		return false
	}
	if len(low) == len(q) {
		return true
	}
	next := low[len(q)]
	return !(next >= 'a' && next <= 'z' || next >= '0' && next <= '9')
}

func sortZIMRankedSlice(s []zimTitleRecord) {
	sort.Slice(s, func(i, j int) bool {
		li, lj := len(s[i].title), len(s[j].title)
		if li != lj {
			return li < lj
		}
		ti, tj := strings.ToLower(s[i].title), strings.ToLower(s[j].title)
		if ti != tj {
			return ti < tj
		}
		return s[i].url < s[j].url
	})
}

func matchZIMTitleTerms(lowTitle, lowURL string, terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(lowTitle, t) && !strings.Contains(lowURL, t) {
			return false
		}
	}
	return true
}

func rankZIMTitleMatches(candidates []zimTitleRecord, q string, terms []string, limit int) []zimTitleRecord {
	var exact, wordPrefix, prefix, phrase, wordMatch []zimTitleRecord
	seen := make(map[string]bool, len(candidates))
	for _, rec := range candidates {
		if seen[rec.url] {
			continue
		}
		low := strings.ToLower(rec.title)
		lowURL := strings.ToLower(strings.ReplaceAll(rec.url, "_", " "))
		if !matchZIMTitleTerms(low, lowURL, terms) {
			continue
		}
		seen[rec.url] = true
		switch {
		case low == q || lowURL == q:
			exact = append(exact, rec)
		case isWordBoundaryPrefix(low, q) || isWordBoundaryPrefix(lowURL, q):
			wordPrefix = append(wordPrefix, rec)
		case strings.HasPrefix(low, q) || strings.HasPrefix(lowURL, q):
			prefix = append(prefix, rec)
		case strings.Contains(low, q) || strings.Contains(lowURL, q):
			phrase = append(phrase, rec)
		default:
			wordMatch = append(wordMatch, rec)
		}
	}
	sortZIMRankedSlice(exact)
	sortZIMRankedSlice(wordPrefix)
	sortZIMRankedSlice(prefix)
	sortZIMRankedSlice(phrase)
	sortZIMRankedSlice(wordMatch)

	out := make([]zimTitleRecord, 0, limit)
	for _, bucket := range [][]zimTitleRecord{exact, wordPrefix, prefix, phrase, wordMatch} {
		if len(out) >= limit {
			break
		}
		out = append(out, bucket...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func titleCaseQuery(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// searchLargeZIMByTitlePtr uses O(log N) binary search over TitlePtrPos so multi-million-entry
// ZIM files return ranked exact, redirect, prefix, and multi-term matches in milliseconds with ~0 RAM.
func (a *zimArchive) searchLargeZIMByTitlePtr(q string, terms []string, limit int) []zimTitleRecord {
	f, err := os.Open(a.path)
	if err != nil {
		return nil
	}
	defer f.Close()

	n := int(a.hdr.ArticleCount)
	wantNS := byte('C')
	if a.hdr.MajorVersion < 6 {
		wantNS = 'A'
	}

	probeSeeds := []string{q}
	if len(terms) > 0 && terms[0] != q {
		probeSeeds = append(probeSeeds, terms[0])
	}
	var probes []string
	seenProbe := make(map[string]bool)
	addProbe := func(s string) {
		if s != "" && !seenProbe[s] {
			seenProbe[s] = true
			probes = append(probes, s)
		}
	}
	for _, s := range probeSeeds {
		addProbe(s)
		addProbe(strings.ToUpper(s[:1]) + s[1:])
		addProbe(titleCaseQuery(s))
		addProbe(strings.ToUpper(s))
	}

	seenURL := make(map[string]bool)
	var collected []zimTitleRecord
	addEntry := func(e zimResolvedEntry) {
		if !a.isSearchableArticle(e.ns, wantNS, e.mimeID, e.url) {
			return
		}
		key := string(e.ns) + ":" + e.url
		if seenURL[key] {
			return
		}
		low := strings.ToLower(e.title)
		lowURL := strings.ToLower(strings.ReplaceAll(e.url, "_", " "))
		if !matchZIMTitleTerms(low, lowURL, terms) {
			return
		}
		seenURL[key] = true
		collected = append(collected, zimTitleRecord{ns: e.ns, url: e.url, title: e.title})
	}

	collectWindow := func(pos int, capCount int) {
		start := pos - 160
		if start < 0 {
			start = 0
		}
		end := pos + 1800
		if end > n {
			end = n
		}
		for i := start; i < end && len(collected) < capCount; i++ {
			urlIdx, err := a.readTitleEntryIdx(f, uint32(i))
			if err != nil {
				continue
			}
			e, err := a.readEntryAt(f, urlIdx)
			if err != nil || e.ns != wantNS {
				continue
			}
			addEntry(e)
		}
	}

	maxCollect := limit * 16
	if maxCollect < 400 {
		maxCollect = 400
	}
	// Run both case-folded binary search (libzim modern) and exact ASCII binary search (legacy libzim)
	for _, s := range probeSeeds {
		if len(collected) >= maxCollect {
			break
		}
		lowS := strings.ToLower(s)
		pos := sort.Search(n, func(i int) bool {
			urlIdx, err := a.readTitleEntryIdx(f, uint32(i))
			if err != nil {
				return false
			}
			e, err := a.readEntryAt(f, urlIdx)
			if err != nil {
				return false
			}
			if e.ns != wantNS {
				return e.ns > wantNS
			}
			return strings.ToLower(e.title) >= lowS
		})
		collectWindow(pos, maxCollect)
	}
	for _, p := range probes {
		if len(collected) >= maxCollect {
			break
		}
		pos := sort.Search(n, func(i int) bool {
			urlIdx, err := a.readTitleEntryIdx(f, uint32(i))
			if err != nil {
				return false
			}
			e, err := a.readEntryAt(f, urlIdx)
			if err != nil {
				return false
			}
			if e.ns != wantNS {
				return e.ns > wantNS
			}
			return e.title >= p
		})
		collectWindow(pos, maxCollect)
	}

	return rankZIMTitleMatches(collected, q, terms, limit)
}

// scanTitlesStreaming searches large v6.3 ZIM files that omit TitlePtrPos (0xffffffffffffffff)
// using a full sequential buffered scan (~130ms for ~877k entries) without pinning all titles in RAM.
func (a *zimArchive) scanTitlesStreaming(q string, terms []string, limit int) []zimTitleRecord {
	f, err := os.Open(a.path)
	if err != nil {
		return nil
	}
	defer f.Close()

	n := int(a.hdr.ArticleCount)
	if n == 0 {
		return nil
	}
	urlPtrs := make([]uint64, n)
	if _, err := f.Seek(int64(a.hdr.URLPtrPos), io.SeekStart); err != nil {
		return nil
	}
	if err := binary.Read(bufio.NewReaderSize(f, 128*1024), binary.LittleEndian, urlPtrs); err != nil {
		return nil
	}
	sort.Slice(urlPtrs, func(i, j int) bool { return urlPtrs[i] < urlPtrs[j] })

	wantNS := byte('C')
	if a.hdr.MajorVersion < 6 {
		wantNS = 'A'
	}

	br := bufio.NewReaderSize(f, 512*1024)
	var curPos uint64 = ^uint64(0)
	var fixed [16]byte
	var exact, wordPrefix, prefix, phrase, wordMatch []zimTitleRecord
	seen := make(map[string]bool)
	const maxBucket = 4000

	for _, ptr := range urlPtrs {
		if curPos != ptr {
			if ptr > curPos && ptr-curPos <= 64*1024 {
				disc, derr := br.Discard(int(ptr - curPos))
				curPos += uint64(disc)
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
		if _, err := io.ReadFull(br, fixed[:2]); err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += 2
		mimeID := binary.LittleEndian.Uint16(fixed[:2])
		if mimeID == 0xfffd || mimeID == 0xfffe {
			continue
		}
		hdrRest := 14
		if mimeID == 0xffff {
			hdrRest = 10
		}
		if _, err := io.ReadFull(br, fixed[2:2+hdrRest]); err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += uint64(hdrRest)
		ns := fixed[3]
		uBytes, err := br.ReadBytes(0)
		if err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += uint64(len(uBytes))
		tBytes, err := br.ReadBytes(0)
		if err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += uint64(len(tBytes))

		uStr := string(bytes.TrimSuffix(uBytes, []byte{0}))
		if !a.isSearchableArticle(ns, wantNS, mimeID, uStr) {
			continue
		}
		tStr := string(bytes.TrimSuffix(tBytes, []byte{0}))
		if tStr == "" {
			tStr = uStr
		}
		low := strings.ToLower(tStr)
		lowURL := strings.ToLower(strings.ReplaceAll(uStr, "_", " "))
		if !matchZIMTitleTerms(low, lowURL, terms) || seen[uStr] {
			continue
		}
		seen[uStr] = true
		rec := zimTitleRecord{ns: ns, url: uStr, title: tStr}
		switch {
		case low == q || lowURL == q:
			exact = append(exact, rec)
		case isWordBoundaryPrefix(low, q) || isWordBoundaryPrefix(lowURL, q):
			if len(wordPrefix) < maxBucket {
				wordPrefix = append(wordPrefix, rec)
			}
		case strings.HasPrefix(low, q) || strings.HasPrefix(lowURL, q):
			if len(prefix) < maxBucket {
				prefix = append(prefix, rec)
			}
		case strings.Contains(low, q) || strings.Contains(lowURL, q):
			if len(phrase) < maxBucket {
				phrase = append(phrase, rec)
			}
		default:
			if len(wordMatch) < maxBucket {
				wordMatch = append(wordMatch, rec)
			}
		}
	}

	sortZIMRankedSlice(exact)
	sortZIMRankedSlice(wordPrefix)
	sortZIMRankedSlice(prefix)
	sortZIMRankedSlice(phrase)
	sortZIMRankedSlice(wordMatch)

	out := make([]zimTitleRecord, 0, limit)
	for _, bucket := range [][]zimTitleRecord{exact, wordPrefix, prefix, phrase, wordMatch} {
		if len(out) >= limit {
			break
		}
		out = append(out, bucket...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// listPages returns a paginated slice of real HTML pages in the archive (excluding redirects and assets).
func (a *zimArchive) listPages(offset, limit int, filter string) ([]zimTitleRecord, int, bool) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	filter = strings.TrimSpace(filter)
	if filter != "" {
		hits := a.searchTitles(filter, offset+limit+1)
		total := len(hits)
		hasMore := total > offset+limit
		if offset >= total {
			return nil, total, false
		}
		end := offset + limit
		if end > total {
			end = total
		}
		return hits[offset:end], total, hasMore
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.hdr.ArticleCount <= 40000 {
		a.buildSmallTitleIndexLocked()
		sorted := make([]zimTitleRecord, 0, len(a.titles))
		for _, r := range a.titles {
			if !isLikelyAssetURL(r.url) {
				sorted = append(sorted, r)
			}
		}
		sort.Slice(sorted, func(i, j int) bool {
			ti, tj := strings.ToLower(sorted[i].title), strings.ToLower(sorted[j].title)
			if ti != tj {
				return ti < tj
			}
			return sorted[i].url < sorted[j].url
		})
		total := len(sorted)
		if offset >= total {
			return nil, total, false
		}
		end := offset + limit
		hasMore := end < total
		if end > total {
			end = total
		}
		return sorted[offset:end], total, hasMore
	}

	// Large ZIM (> 40,000 entries): stream URLPtrPos sequentially in offset order without pinning all entries in RAM.
	f, err := os.Open(a.path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()

	n := int(a.hdr.ArticleCount)
	if n == 0 {
		return nil, 0, false
	}
	urlPtrs := make([]uint64, n)
	if _, err := f.Seek(int64(a.hdr.URLPtrPos), io.SeekStart); err != nil {
		return nil, 0, false
	}
	if err := binary.Read(bufio.NewReaderSize(f, 128*1024), binary.LittleEndian, urlPtrs); err != nil {
		return nil, 0, false
	}
	sort.Slice(urlPtrs, func(i, j int) bool { return urlPtrs[i] < urlPtrs[j] })

	wantNS := byte('C')
	if a.hdr.MajorVersion < 6 {
		wantNS = 'A'
	}

	br := bufio.NewReaderSize(f, 512*1024)
	var curPos uint64 = ^uint64(0)
	var fixed [16]byte
	pageIdx := 0
	pages := make([]zimTitleRecord, 0, limit)
	for _, ptr := range urlPtrs {
		if curPos != ptr {
			if ptr > curPos && ptr-curPos <= 64*1024 {
				disc, derr := br.Discard(int(ptr - curPos))
				curPos += uint64(disc)
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
		if _, err := io.ReadFull(br, fixed[:2]); err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += 2
		mimeID := binary.LittleEndian.Uint16(fixed[:2])
		if mimeID >= 0xfffd {
			continue
		}
		if _, err := io.ReadFull(br, fixed[2:16]); err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += 14
		ns := fixed[3]
		uBytes, err := br.ReadBytes(0)
		if err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += uint64(len(uBytes))
		tBytes, err := br.ReadBytes(0)
		if err != nil {
			curPos = ^uint64(0)
			continue
		}
		curPos += uint64(len(tBytes))

		if ns != wantNS || int(mimeID) >= len(a.mimes) || !strings.HasPrefix(strings.ToLower(a.mimes[mimeID]), "text/html") {
			continue
		}
		uStr := string(bytes.TrimSuffix(uBytes, []byte{0}))
		if isLikelyAssetURL(uStr) {
			continue
		}
		tStr := string(bytes.TrimSuffix(tBytes, []byte{0}))
		if tStr == "" {
			tStr = uStr
		}
		if pageIdx >= offset && len(pages) < limit {
			pages = append(pages, zimTitleRecord{ns: ns, url: uStr, title: tStr})
		}
		pageIdx++
	}
	return pages, pageIdx, offset+len(pages) < pageIdx
}

func (a *zimArchive) resolveFrontPageEntry(f *os.File) (zimResolvedEntry, bool) {
	if a.hdr.ArticleCount == 0 {
		return zimResolvedEntry{}, false
	}
	if a.hdr.MainPage != 0xffffffff && a.hdr.MainPage < a.hdr.ArticleCount {
		if entry, err := a.readEntryAt(f, a.hdr.MainPage); err == nil {
			if resolved, rerr := a.followRedirects(f, entry); rerr == nil && resolved.mimeID < 0xfffd {
				return resolved, true
			}
		}
	}
	for _, cand := range []string{"mainPage", "W/mainPage", "Main_Page", "index", "index.html", "index.htm", "Home", "Welcome"} {
		if entry, ok := a.findEntryFlex(f, cand); ok {
			if resolved, rerr := a.followRedirects(f, entry); rerr == nil && resolved.mimeID < 0xfffd {
				return resolved, true
			}
		}
	}
	limit := a.hdr.ArticleCount
	if limit > 64 {
		limit = 64
	}
	for idx := uint32(0); idx < limit; idx++ {
		e, err := a.readEntryAt(f, idx)
		if err != nil {
			continue
		}
		if e.mimeID == 0xffff {
			if resolved, rerr := a.followRedirects(f, e); rerr == nil {
				e = resolved
			}
		}
		if e.mimeID < 0xfffd && (e.ns == 'C' || e.ns == 'A') && int(e.mimeID) < len(a.mimes) {
			if strings.HasPrefix(strings.ToLower(a.mimes[e.mimeID]), "text/html") {
				return e, true
			}
		}
	}
	return zimResolvedEntry{}, false
}

func (a *zimArchive) findEntryFlex(f *os.File, subPath string) (zimResolvedEntry, bool) {
	_, e, ok := a.findEntryFlexWithIndex(f, subPath)
	return e, ok
}

func (a *zimArchive) findEntryFlexWithIndex(f *os.File, subPath string) (int, zimResolvedEntry, bool) {
	candidates := []string{subPath}
	if strings.Contains(subPath, " ") {
		candidates = append(candidates, strings.ReplaceAll(subPath, " ", "_"))
	}
	for _, cand := range candidates {
		cand = strings.TrimPrefix(cand, "/")
		if len(cand) >= 2 && cand[1] == '/' {
			ns := cand[0]
			rest := cand[2:]
			if idx, e, ok := a.findEntryWithIndex(f, ns, rest); ok {
				return idx, e, true
			}
			// ZIM v6 places -/... and I/... under namespace 'C'
			if idx, e, ok := a.findEntryWithIndex(f, 'C', cand); ok {
				return idx, e, true
			}
		}
		for _, tryNS := range []byte{'C', 'A', '-', 'I', 'W'} {
			if idx, e, ok := a.findEntryWithIndex(f, tryNS, cand); ok {
				return idx, e, true
			}
		}
	}
	return -1, zimResolvedEntry{}, false
}

// resolveZIMArticleURL verifies whether stemHint/articleHint resolves to a real entry
// inside local ZIM archives and returns the canonical /read/zim/<stem>/<ns>/<url> path.
func (s *Server) resolveZIMArticleURL(stemHint, articleHint string) (string, bool) {
	if s.dataDir == "" || strings.TrimSpace(articleHint) == "" {
		return "", false
	}
	stems := s.listLocalZIMStems()
	if len(stems) == 0 {
		return "", false
	}

	var tryStems []string
	if stemHint != "" {
		cleanStem := filepath.Base(stemHint)
		for _, st := range stems {
			if strings.EqualFold(st, cleanStem) {
				tryStems = append(tryStems, st)
				break
			}
		}
	}
	if len(tryStems) == 0 {
		tryStems = stems
	}

	subPath, frag, hasFrag := strings.Cut(articleHint, "#")
	if unesc, err := url.PathUnescape(subPath); err == nil {
		subPath = unesc
	}
	subPath = strings.TrimPrefix(subPath, "/")

	for _, st := range tryStems {
		zimPath := filepath.Join(s.dataDir, "zim", st+".zim")
		arc, err := getZIMArchive(zimPath, st)
		if err != nil {
			continue
		}
		f, err := os.Open(arc.path)
		if err != nil {
			continue
		}
		arc.mu.Lock()
		entry, found := arc.findEntryFlex(f, subPath)
		if found && entry.mimeID == 0xffff {
			if redir, rerr := arc.followRedirects(f, entry); rerr == nil {
				entry = redir
			}
		}
		arc.mu.Unlock()
		_ = f.Close()

		if found && entry.mimeID < 0xfffd {
			targetPath, targetFrag, tHasFrag := strings.Cut(entry.url, "#")
			resolved := fmt.Sprintf("/read/zim/%s/%c/%s", url.PathEscape(st), entry.ns, targetPath)
			if tHasFrag && targetFrag != "" {
				resolved += "#" + targetFrag
			} else if hasFrag && frag != "" {
				resolved += "#" + frag
			}
			return resolved, true
		}
	}
	return "", false
}

// readZIMArticlePlainText reads a specific article's cleaned plain text directly from a local ZIM file.
func (s *Server) readZIMArticlePlainText(stemHint, articleHint string, maxRunes int) (title, plain string, resolvedPath string, ok bool) {
	if s.dataDir == "" || strings.TrimSpace(stemHint) == "" || strings.TrimSpace(articleHint) == "" {
		return "", "", "", false
	}
	resolved, rok := s.resolveZIMArticleURL(stemHint, articleHint)
	if !rok {
		return "", "", "", false
	}
	// resolved has form /read/zim/<stem>/<subPath>
	cleanPath, _, _ := strings.Cut(resolved, "?")
	cleanPath = strings.TrimPrefix(cleanPath, "/read/zim/")
	slash := strings.IndexByte(cleanPath, '/')
	if slash <= 0 {
		return "", "", "", false
	}
	stem := filepath.Base(cleanPath[:slash])
	if u, err := url.PathUnescape(stem); err == nil {
		stem = u
	}
	subPath := cleanPath[slash+1:]
	if u, err := url.PathUnescape(subPath); err == nil {
		subPath = u
	}
	zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
	arc, err := getZIMArchive(zimPath, stem)
	if err != nil {
		return "", "", "", false
	}
	f, err := os.Open(arc.path)
	if err != nil {
		return "", "", "", false
	}
	defer f.Close()

	arc.mu.Lock()
	entry, found := arc.findEntryFlex(f, subPath)
	if found && entry.mimeID == 0xffff {
		if redir, rerr := arc.followRedirects(f, entry); rerr == nil {
			entry = redir
		}
	}
	var blob []byte
	if found && entry.mimeID < 0xfffd {
		blob, _ = arc.readBlob(f, entry.clusterNum, entry.blobNum)
	}
	arc.mu.Unlock()
	if !found || len(blob) == 0 {
		return "", "", "", false
	}
	t, txt := ai.HTMLToPlainText(string(blob), entry.title, maxRunes)
	if t == "" {
		t = subPath
	}
	return t, txt, resolved, strings.TrimSpace(txt) != ""
}

func (s *Server) listLocalZIMStems() []string {
	if s.dataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "zim"))
	if err != nil {
		return nil
	}
	var stems []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".zim") {
			stems = append(stems, strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		}
	}
	sort.Strings(stems)
	return stems
}

func (s *Server) hasLocalZIMFiles() bool {
	return len(s.listLocalZIMStems()) > 0
}

func (s *Server) isZIMIndexed(zimPath string) bool {
	if s.db == nil {
		return false
	}
	cleanP := filepath.Clean(zimPath)
	slashP := filepath.ToSlash(cleanP)
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM rag_documents WHERE source_path IN (?, ?, ?) LIMIT 1`, zimPath, cleanP, slashP).Scan(&one)
	return err == nil && one == 1
}

func (s *Server) renderZIMStartPage(w http.ResponseWriter, stem, zimPath string, arc *zimArchive) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	escStem := url.PathEscape(stem)
	displayTitle, metaDesc := arc.metadata()
	if displayTitle == "" {
		displayTitle = strings.ReplaceAll(stem, "_", " ")
	}
	_ = metaDesc
	iconURL := "/api/library/icon?file=" + url.QueryEscape(stem+".zim") + "&cat=ZIM"
	aiBadge := ""
	if s.isZIMIndexed(zimPath) {
		aiBadge = `<span style="display:inline-flex;align-items:center;gap:6px;padding:4px 10px;border-radius:999px;font-size:12px;font-weight:600;border:1px solid var(--ark-border, #DCD6CC);background:var(--ark-surface, #FDFBF7);">Indexed in Ask AI</span>`
	} else {
		aiBadge = `<button type="button" id="ark-home-add-ai" onclick="var b=this;b.disabled=true;b.textContent='Queueing...';fetch('/api/ai/index',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded','Accept':'application/json'},body:'path='+encodeURIComponent('zim/` + html.EscapeString(stem+".zim") + `')+'&ajax=1'}).then(r=>r.json()).then(d=>{b.textContent=d.ok?'Indexing in AI...':('Error: '+(d.error||'failed'));}).catch(()=>{b.textContent='Error';});" style="padding:5px 12px;border-radius:999px;border:none;background:#C25E28;color:#FDFBF7;font-size:12px;font-weight:600;cursor:pointer;">+ Add Archive to AI</button>`
	}

	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s — Ark Reader</title>
%s
<style>
  body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; min-height: 100vh; display: flex; flex-direction: column; }
  .wiki-home { max-width: 720px; width: 100%%; margin: 5vh auto 3rem; padding: 0 1.5rem; box-sizing: border-box; text-align: center; }
  .wiki-globe { width: 84px; height: 84px; border-radius: 16px; object-fit: contain; padding: 10px; background: var(--ark-surface, #FDFBF7); border: 1px solid var(--ark-border, #DCD6CC); box-shadow: 0 6px 20px rgba(0,0,0,0.06); margin-bottom: 1rem; }
  .wiki-title { font-family: Georgia, 'Times New Roman', serif; font-size: 32px; font-weight: 700; margin: 0 0 0.35rem; letter-spacing: -0.01em; }
  .wiki-sub { font-size: 14px; opacity: 0.78; margin: 0 0 1.75rem; display: flex; gap: 10px; justify-content: center; align-items: center; flex-wrap: wrap; }
  .wiki-search-box { display: flex; gap: 8px; max-width: 580px; margin: 0 auto 1.25rem; }
  .wiki-search-box input { flex: 1; padding: 0.75rem 1rem; font-size: 16px; border: 1px solid var(--ark-border, #DCD6CC); border-radius: 8px; background: var(--ark-surface, #FDFBF7); color: inherit; }
  .wiki-search-box button { padding: 0.75rem 1.35rem; font-size: 15px; font-weight: 600; border: none; border-radius: 8px; background: #C25E28; color: #FDFBF7; cursor: pointer; }
  .wiki-actions { display: flex; gap: 12px; justify-content: center; flex-wrap: wrap; margin-top: 1.5rem; font-size: 14px; }
  .wiki-actions a { color: #C25E28; text-decoration: none; font-weight: 600; }
  .wiki-actions a:hover { text-decoration: underline; }
</style>
</head>
<body>
%s
<main id="ark-zim-home" class="wiki-home">
  <img src="%s" alt="" class="wiki-globe">
  <h1 class="wiki-title">%s</h1>
  <div class="wiki-sub">
    <span>%d indexed entries</span>
    <span>&middot;</span>
    <span>Local Offline Archive</span>
    %s
  </div>
  <form action="/read/zim/%s/" method="GET" class="wiki-search-box">
    <input type="search" name="q" placeholder="Search articles in %s..." autofocus required>
    <button type="submit">Search</button>
  </form>
  <div class="wiki-actions">
    <a href="/read/zim/%s/__main__">Original Archive Home &rarr;</a>
    <span>&middot;</span>
    <a href="/library">All Library Items</a>
    <span>&middot;</span>
    <a href="/ask">Ask Local AI</a>
  </div>
</main>
%s
</body>
</html>`,
		html.EscapeString(displayTitle),
		arkThemeHeadTag(),
		buildReaderNavHTML(stem, true),
		html.EscapeString(iconURL),
		html.EscapeString(displayTitle),
		arc.hdr.ArticleCount,
		aiBadge,
		escStem,
		html.EscapeString(displayTitle),
		escStem,
		arkThemeScriptTag(s.getSavedReaderTheme()),
	)
}

func (s *Server) handleZIMReader(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/reader/theme") {
		s.handleAPIReaderTheme(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.dataDir == "" {
		http.NotFound(w, r)
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/read/zim")
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		s.renderLocalZIMIndex(w)
		return
	}

	parts := strings.SplitN(trimmed, "/", 2)
	stem := filepath.Base(parts[0])
	if stem == "" || stem == "." || stem == ".." {
		http.NotFound(w, r)
		return
	}

	zimPath := filepath.Join(s.dataDir, "zim", stem+".zim")
	arc, err := getZIMArchive(zimPath, stem)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Search query on /read/zim/<stem>/?q=...
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		hits := arc.searchTitles(q, 50)
		s.renderZIMSearchResults(w, stem, q, hits, s.isZIMIndexed(zimPath))
		return
	}

	subPath := ""
	if len(parts) == 2 {
		subPath = parts[1]
	}
	subPath = strings.TrimPrefix(subPath, "/")

	f, err := os.Open(arc.path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	if subPath == "" || subPath == "__main__" {
		arc.mu.Lock()
		mainEntry, ok := arc.resolveFrontPageEntry(f)
		arc.mu.Unlock()
		if !ok {
			s.renderZIMStartPage(w, stem, zimPath, arc)
			return
		}
		targetPath, targetFrag, hasFrag := strings.Cut(mainEntry.url, "#")
		target := fmt.Sprintf("/read/zim/%s/%c/%s", url.PathEscape(stem), mainEntry.ns, targetPath)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		if hasFrag && targetFrag != "" {
			target += "#" + targetFrag
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	arc.mu.Lock()
	entryIdx, entry, found := arc.findEntryFlexWithIndex(f, subPath)
	if !found {
		arc.mu.Unlock()
		http.NotFound(w, r)
		return
	}

	if entry.mimeID == 0xffff {
		targetEntry, rerr := arc.followRedirects(f, entry)
		arc.mu.Unlock()
		if rerr != nil {
			http.NotFound(w, r)
			return
		}
		targetPath, targetFrag, hasFrag := strings.Cut(targetEntry.url, "#")
		target := fmt.Sprintf("/read/zim/%s/%c/%s", url.PathEscape(stem), targetEntry.ns, targetPath)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		if hasFrag && targetFrag != "" {
			target += "#" + targetFrag
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	if entry.mimeID >= 0xfffd {
		arc.mu.Unlock()
		http.NotFound(w, r)
		return
	}

	blob, berr := arc.readBlob(f, entry.clusterNum, entry.blobNum)
	mimeType := "application/octet-stream"
	if int(entry.mimeID) < len(arc.mimes) && arc.mimes[entry.mimeID] != "" {
		mimeType = arc.mimes[entry.mimeID]
	}
	isHTML := strings.Contains(strings.ToLower(mimeType), "text/html")
	if isHTML && berr == nil && r.URL.Query().Get("download") == "1" {
		standalone := inlineZIMPageAssets(arc, f, entry, blob)
		label := formatSubpageLabel(stem, entry, blob)
		arc.mu.Unlock()
		fileName := sanitizeDownloadHTMLFilename(label, entry.url)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		_, _ = w.Write(standalone)
		return
	}

	var prevPage, nextPage *zimTitleRecord
	if isHTML && berr == nil {
		prevPage, nextPage = arc.adjacentHTMLPages(f, entryIdx, entry.ns)
	}
	arc.mu.Unlock()
	if berr != nil {
		http.Error(w, "Failed to read ZIM content: "+berr.Error(), http.StatusInternalServerError)
		return
	}

	if isHTML {
		if r.URL.RawQuery != "" {
			blob = preserveQueryInMetaRefresh(blob, r.URL.RawQuery)
		}
		curLabel := formatSubpageLabel(stem, entry, blob)
		blob = injectZIMReaderNav(blob, stem, s.isZIMIndexed(zimPath), prevPage, nextPage, curLabel, s.getSavedReaderTheme())
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", mimeType)
		_, _ = w.Write(blob)
		return
	}

	// Serve non-HTML ZIM assets (videos, audio, PDFs, images, CSS, JS) via http.ServeContent
	// so HTTP Range / 206 Partial Content seeking & scrubbing work natively.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", mimeType)
	http.ServeContent(w, r, path.Base(entry.url), time.Unix(0, arc.modUnixNano), bytes.NewReader(blob))
}

func arkThemeHeadTag() string {
	return `<meta charset="utf-8"><style id="ark-reader-base">
.ark-ai-highlight { background: #FDE047 !important; color: #1B2E26 !important; padding: 1px 4px; border-radius: 3px; outline: 2px solid #C25E28; }
.ark-ai-highlight-block { background: rgba(253, 224, 71, 0.25) !important; border-left: 4px solid #C25E28 !important; padding: 4px 8px !important; border-radius: 4px; }
html { scroll-padding-top: 54px !important; }
html body { padding-top: 46px !important; }
#container { top: 46px !important; height: calc(100% - 46px) !important; }
.item figcaption, .item figcaption * { color: #FFFFFF !important; text-shadow: 0 1px 3px rgba(0,0,0,0.85) !important; }
#ark-proxy-nav { position: fixed !important; top: 0 !important; left: 0 !important; right: 0 !important; width: 100% !important; height: 46px !important; min-height: 46px !important; max-height: 46px !important; flex: none !important; align-self: flex-start !important; box-sizing: border-box !important; z-index: 2147483647 !important; }
#ark-page-pager { position: relative !important; left: auto !important; right: auto !important; top: auto !important; bottom: auto !important; overflow: visible !important; display: flex !important; flex-direction: column !important; align-items: center !important; justify-content: center !important; gap: 8px !important; width: 100% !important; margin: 2rem auto 2.5rem !important; padding: 0.75rem 1rem !important; box-sizing: border-box !important; clear: both !important; visibility: visible !important; opacity: 1 !important; }
#ark-page-pager a, #ark-page-pager a * { color: #F5F2EB !important; }
#ark-proxy-nav input, #ark-proxy-nav select { background-color: #12201A !important; color: #F5F2EB !important; caret-color: #F5F2EB !important; border-color: #436153 !important; -webkit-text-fill-color: #F5F2EB !important; }
#ark-proxy-nav input::placeholder { color: #A9BEB3 !important; opacity: 1 !important; -webkit-text-fill-color: #A9BEB3 !important; }
</style>`
}

var metaRefreshURLRe = regexp.MustCompile(`(?is)(<meta[^>]+http-equiv=["']?refresh["']?[^>]+content=["']?\d+\s*;\s*url=['"]?)([^"'>\s]+)`)

func preserveQueryInMetaRefresh(htmlBytes []byte, rawQuery string) []byte {
	if len(htmlBytes) > 4096 || rawQuery == "" {
		return htmlBytes
	}
	return metaRefreshURLRe.ReplaceAllFunc(htmlBytes, func(match []byte) []byte {
		sub := metaRefreshURLRe.FindSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		prefix := sub[1]
		target := string(sub[2])
		pathPart, frag, hasFrag := strings.Cut(target, "#")
		sep := "?"
		if strings.Contains(pathPart, "?") {
			sep = "&"
		}
		rebuilt := string(prefix) + pathPart + sep + rawQuery
		if hasFrag {
			rebuilt += "#" + frag
		}
		return []byte(rebuilt)
	})
}

func themeRule(bg, surface, fg, link, border string, isDark bool) string {
	mathRule := fmt.Sprintf("math,math *,.mwe-math-element,.mwe-math-element *{color:%s!important;}", fg)
	if isDark {
		mathRule += "img.mwe-math-fallback-image-inline,img.mwe-math-fallback-image-display,.mw-invert,.skin-invert{filter:invert(1) hue-rotate(180deg)!important;}"
	} else {
		mathRule += "img.mwe-math-fallback-image-inline,img.mwe-math-fallback-image-display,.mw-invert,.skin-invert{filter:none!important;}"
	}
	scheme := "light"
	if isDark {
		scheme = "dark"
	}
	return fmt.Sprintf(
		":root,html,html.skin-theme-clientpref-day,html.skin-theme-clientpref-night,html.skin-theme-clientpref-os{color-scheme:%s;--background-color-base:%s;--background-color-base-fixed:%s;--background-color-neutral:%s;--background-color-neutral-subtle:%s;--background-color-interactive:%s;--background-color-interactive-subtle:%s;--color-base:%s;--color-emphasized:%s;--color-subtle:%s;--color-progressive:%s;--color-link:%s;--border-color-base:%s;--border-color-subtle:%s;}"+
			"html,body,#content,.mw-body,.mw-page-container,.mw-body-content,main,article{background:%s!important;color:%s!important;}"+
			"body :not(#ark-proxy-nav):not(#ark-proxy-nav *){color:%s;}"+
			"h1,h2,h3,h4,h5,h6,.mw-heading,.mw-heading *{background:transparent!important;color:%s!important;}"+
			"a:not(#ark-proxy-nav a):not(#ark-page-pager a){color:%s!important;}"+
			"#ark-proxy-nav a,#ark-page-pager a{color:#F5F2EB!important;}"+
			"#ark-proxy-nav input,#ark-proxy-nav select{background-color:#12201A!important;color:#F5F2EB!important;caret-color:#F5F2EB!important;border-color:#436153!important;-webkit-text-fill-color:#F5F2EB!important;}"+
			"#ark-proxy-nav input::placeholder{color:#A9BEB3!important;opacity:1!important;-webkit-text-fill-color:#A9BEB3!important;}"+
			"html body input:not(#ark-proxy-nav input),html body select:not(#ark-proxy-nav select),html body textarea:not(#ark-proxy-nav textarea),html body .form-control{background-color:%s!important;color:%s!important;caret-color:%s!important;border-color:%s!important;-webkit-text-fill-color:%s!important;}"+
			"html body input:not(#ark-proxy-nav input)::placeholder,html body textarea:not(#ark-proxy-nav textarea)::placeholder{color:%s!important;opacity:0.72!important;-webkit-text-fill-color:%s!important;}"+
			"html body table,html body .infobox,html body .infobox *,html body .sidebar,html body .sidebar *,html body .navbox,html body .navbox-title,html body .sidebar-title,html body .sidebar-list-title,html body .hatnote,html body div.hatnote,html body .toc,html body th,html body td,html body caption,html body pre,html body code{background-color:%s!important;color:%s!important;border-color:%s!important;}"+
			"html body .sidebar a,html body .infobox a,html body .hatnote a{color:%s!important;}"+
			"html body .item figcaption,html body .item figcaption *{color:#FFFFFF!important;text-shadow:0 1px 3px rgba(0,0,0,0.85)!important;}"+
			"%s",
		scheme, bg, bg, surface, surface, surface, surface, fg, fg, fg, link, link, border, border,
		bg, fg, fg, fg, link,
		surface, fg, fg, border, fg, fg, fg,
		surface, fg, border, link, mathRule,
	)
}

func validReaderTheme(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "forest", "light", "sepia", "dark", "default":
		return strings.ToLower(strings.TrimSpace(t))
	default:
		return "forest"
	}
}

func (s *Server) getSavedReaderTheme() string {
	if s.db == nil {
		return "forest"
	}
	var val string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = 'reader_theme'`).Scan(&val); err == nil {
		return validReaderTheme(val)
	}
	return "forest"
}

func (s *Server) handleAPIReaderTheme(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]string{"theme": s.getSavedReaderTheme()})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var theme string
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var payload struct {
			Theme string `json:"theme"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&payload); err == nil {
			theme = payload.Theme
		}
	} else {
		theme = r.FormValue("theme")
	}
	theme = validReaderTheme(theme)
	if s.db != nil {
		_, _ = s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('reader_theme', ?)`, theme)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "theme": theme})
}

func arkThemeScriptTag(initialTheme string) string {
	initialTheme = validReaderTheme(initialTheme)
	return `<style id="ark-reader-theme"></style><script>
(function() {
  var THEMES = {
    default: '#ark-proxy-nav a{color:#F5F2EB!important;}#ark-proxy-nav input,#ark-proxy-nav select{background-color:#12201A!important;color:#F5F2EB!important;caret-color:#F5F2EB!important;border-color:#436153!important;-webkit-text-fill-color:#F5F2EB!important;}#ark-proxy-nav input::placeholder{color:#A9BEB3!important;opacity:1!important;-webkit-text-fill-color:#A9BEB3!important;}',
    light: ` + fmt.Sprintf("%q", themeRule("#FFFFFF", "#F6F8FA", "#1F2328", "#0969DA", "#D0D7DE", false)) + `,
    sepia: ` + fmt.Sprintf("%q", themeRule("#F5EFE2", "#ECE3D2", "#3D3022", "#9E471C", "#CFC3AC", false)) + `,
    forest: ` + fmt.Sprintf("%q", themeRule("#F2F5F1", "#E6ECE4", "#1B2E26", "#C25E28", "#C5D1C2", false)) + `,
    dark: ` + fmt.Sprintf("%q", themeRule("#121715", "#1B221F", "#E6ECE8", "#E08A5A", "#2D3A34", true)) + `
  };
  var KEY = 'ark-zim-theme';
  var SERVER_THEME = ` + fmt.Sprintf("%q", initialTheme) + `;
  function applyTheme(t, persist) {
    if (!THEMES.hasOwnProperty(t)) t = 'forest';
    var root = document.documentElement;
    if (root && root.classList) {
      root.classList.remove('skin-theme-clientpref-os', 'skin-theme-clientpref-day', 'skin-theme-clientpref-night');
      if (t === 'dark') {
        root.classList.add('skin-theme-clientpref-night');
      } else if (t === 'default') {
        root.classList.add('skin-theme-clientpref-os');
      } else {
        root.classList.add('skin-theme-clientpref-day');
      }
    }
    var styleEl = document.getElementById('ark-reader-theme');
    if (styleEl) styleEl.textContent = THEMES[t];
    var sel = document.getElementById('ark-theme-sel');
    if (sel && sel.value !== t) sel.value = t;
    try { localStorage.setItem(KEY, t); } catch (e) {}
    if (persist) {
      try {
        var doFetch = window.__arkFetch || window.fetch;
        doFetch('/api/reader/theme', {
          method: 'POST',
          keepalive: true,
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ theme: t })
        }).catch(function(){});
      } catch (e) {}
    }
  }
  window.arkSetZIMTheme = applyTheme;
  var initial = SERVER_THEME;
  if (!THEMES.hasOwnProperty(initial)) {
    try { initial = localStorage.getItem(KEY) || 'forest'; } catch (e) { initial = 'forest'; }
  }
  applyTheme(initial, false);
  window.addEventListener('pageshow', function(ev) {
    if (ev && ev.persisted) {
      try {
        fetch(location.origin + '/api/reader/theme')
          .then(function(r) { return r.json(); })
          .then(function(d) { if (d && d.theme) applyTheme(d.theme, false); })
          .catch(function(){});
      } catch (e) {}
    }
  });

  function highlightAIQuote() {
    var hl = new URLSearchParams(location.search).get('ark_hl');
    if (!hl) return;
    var norm = function(s) { return String(s || '').replace(/\s+/g, ' ').trim().toLowerCase(); };
    var needle = norm(hl);
    if (needle.length < 4) return;
    var shortNeedle = needle.length > 52 ? needle.slice(0, 52) : needle;

    var walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    var nav = document.getElementById('ark-proxy-nav');
    var node;
    while ((node = walker.nextNode())) {
      if (nav && nav.contains(node)) continue;
      if (node.parentElement && /^(SCRIPT|STYLE|NOSCRIPT|NAV)$/.test(node.parentElement.tagName)) continue;
      var raw = node.nodeValue || '';
      var low = raw.toLowerCase();
      var idx = low.indexOf(needle);
      var matchLen = needle.length;
      if (idx === -1 && shortNeedle !== needle) {
        idx = low.indexOf(shortNeedle);
        matchLen = shortNeedle.length;
      }
      if (idx !== -1) {
        try {
          var rng = document.createRange();
          rng.setStart(node, idx);
          rng.setEnd(node, Math.min(raw.length, idx + matchLen));
          var mark = document.createElement('mark');
          mark.id = 'ark-ai-hit';
          mark.className = 'ark-ai-highlight';
          rng.surroundContents(mark);
          mark.scrollIntoView({ behavior: 'smooth', block: 'center' });
          return;
        } catch (e) {}
      }
    }

    // Fallback for multi-tag Wikipedia sentences containing inline links
    var blocks = document.querySelectorAll('p, li, dd, dt, blockquote, td, th, h1, h2, h3, h4');
    var sub = needle.length > 36 ? needle.slice(0, 36) : needle;
    for (var i = 0; i < blocks.length; i++) {
      var b = blocks[i];
      if (nav && nav.contains(b)) continue;
      var txt = norm(b.textContent);
      if (txt.indexOf(needle) !== -1 || txt.indexOf(sub) !== -1) {
        b.classList.add('ark-ai-highlight-block');
        b.scrollIntoView({ behavior: 'smooth', block: 'center' });
        return;
      }
    }
  }
  function offsetFixedTopbars() {
    if (!document.body) return;
    var nav = document.getElementById('ark-proxy-nav');
    var navH = nav ? Math.ceil(nav.getBoundingClientRect().height || 46) : 46;
    var seen = [];
    var candidates = [];
    var push = function(el) {
      if (!el || el === nav || el.id === 'ark-page-pager' || (nav && nav.contains(el))) return;
      if (seen.indexOf(el) === -1) { seen.push(el); candidates.push(el); }
    };
    for (var i = 0; i < document.body.children.length; i++) push(document.body.children[i]);
    var q = document.body.querySelectorAll('header, nav, .navbar, .navbar-fixed-top, [class*="header"], [class*="navbar"], [class*="topbar"], [id*="header"], [id*="nav"]');
    for (var j = 0; j < q.length; j++) push(q[j]);

    var maxExtraH = 0;
    for (var k = 0; k < candidates.length; k++) {
      var el = candidates[k];
      if (el.getAttribute('data-ark-shifted') === '1') {
        var rShifted = el.getBoundingClientRect();
        if (rShifted.height > maxExtraH && rShifted.height < 220) maxExtraH = rShifted.height;
        continue;
      }
      var cs = getComputedStyle(el);
      if (cs.position !== 'fixed' && cs.position !== 'sticky') continue;
      var r = el.getBoundingClientRect();
      if (r.height <= 0 || r.height >= 220 || r.top < -2 || r.top >= navH) continue;
      var curTop = parseFloat(cs.top);
      if (isNaN(curTop)) curTop = 0;
      el.setAttribute('data-ark-shifted', '1');
      el.style.setProperty('top', (curTop + navH) + 'px', 'important');
      if (r.height > maxExtraH) maxExtraH = r.height;
    }
    if (maxExtraH > 0) {
      document.body.style.setProperty('padding-top', (navH + Math.ceil(maxExtraH)) + 'px', 'important');
      document.documentElement.style.setProperty('scroll-padding-top', (navH + Math.ceil(maxExtraH) + 8) + 'px', 'important');
    }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function() { highlightAIQuote(); offsetFixedTopbars(); });
  } else {
    highlightAIQuote();
    offsetFixedTopbars();
  }
  window.addEventListener('load', offsetFixedTopbars);
  var ctn = document.getElementById('container');
  var pgr = document.getElementById('ark-page-pager');
  if (ctn && pgr && ctn !== pgr.parentElement) {
    ctn.appendChild(pgr);
  }
  function parseZIMPath() {
    var m = location.pathname.match(/^\/read\/zim\/([^\/]+)\/(.*)$/);
    if (!m) return { stem: '', art: '' };
    return { stem: decodeURIComponent(m[1] || ''), art: decodeURIComponent(m[2] || '') };
  }
  var askLink = document.getElementById('ark-ask-link');
  if (askLink) {
    var zi = parseZIMPath();
    if (zi.stem && zi.art) {
      askLink.href = '/ask?zim=' + encodeURIComponent(zi.stem) + '&article=' + encodeURIComponent(zi.art);
    }
  }
  var pop = null;
  function hidePop() { if (pop) pop.style.display = 'none'; }
  function ensurePop() {
    if (pop) return pop;
    pop = document.createElement('div');
    pop.id = 'ark-sel-popover';
    pop.style.cssText = 'position:fixed;display:none;z-index:2147483647;background:#1B2E26;border:1px solid #436153;border-radius:6px;padding:4px;gap:4px;box-shadow:0 4px 12px rgba(0,0,0,0.28);font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif;';
    var mk = function(label, accent, fn) {
      var b = document.createElement('button');
      b.type = 'button';
      b.textContent = label;
      b.style.cssText = 'border:none;border-radius:4px;padding:4px 9px;font-size:12px;font-weight:600;cursor:pointer;color:#F5F2EB;background:' + (accent ? '#C25E28' : '#2A4338') + ';';
      b.onmousedown = function(ev) { ev.preventDefault(); ev.stopPropagation(); fn(); hidePop(); };
      return b;
    };
    pop.appendChild(mk('Explain', true, function() {
      var t = (window.getSelection() || '').toString().trim().slice(0, 1200);
      if (!t) return;
      var zi = parseZIMPath();
      window.open('/ask?zim=' + encodeURIComponent(zi.stem) + '&article=' + encodeURIComponent(zi.art) + '&q=' + encodeURIComponent('Explain this: ' + t), '_blank', 'noopener');
    }));
    pop.appendChild(mk('Summarize', false, function() {
      var t = (window.getSelection() || '').toString().trim().slice(0, 1200);
      if (!t) return;
      var zi = parseZIMPath();
      window.open('/ask?zim=' + encodeURIComponent(zi.stem) + '&article=' + encodeURIComponent(zi.art) + '&q=' + encodeURIComponent('Summarize this: ' + t), '_blank', 'noopener');
    }));
    pop.appendChild(mk('Translate', false, function() {
      var t = (window.getSelection() || '').toString().trim().slice(0, 1800);
      if (!t) return;
      var base = location.protocol + '//' + location.hostname + ':5000/?source=auto&target=en&q=' + encodeURIComponent(t);
      window.open(base, '_blank', 'noopener');
    }));
    document.body.appendChild(pop);
    return pop;
  }
  document.addEventListener('mouseup', function(ev) {
    var nav = document.getElementById('ark-proxy-nav');
    if (nav && nav.contains(ev.target)) return;
    if (pop && pop.contains(ev.target)) return;
    setTimeout(function() {
      var sel = window.getSelection ? window.getSelection() : null;
      var txt = sel ? sel.toString().trim() : '';
      if (txt.length < 3 || txt.length > 2500 || !sel.rangeCount) { hidePop(); return; }
      var rect = sel.getRangeAt(0).getBoundingClientRect();
      if (!rect || (rect.width === 0 && rect.height === 0)) { hidePop(); return; }
      var el = ensurePop();
      el.style.display = 'flex';
      var top = Math.max(52, rect.top - 40);
      var left = Math.min(Math.max(8, rect.left), window.innerWidth - 230);
      el.style.top = top + 'px';
      el.style.left = left + 'px';
    }, 10);
  });
  document.addEventListener('mousedown', function(ev) {
    if (pop && !pop.contains(ev.target)) hidePop();
  });

  // Per-item collapsible left Table of Contents / Pages panel (collapsed by default)
  (function initZIMTOC() {
    var panel = document.getElementById('ark-toc-panel');
    var toggleBtn = document.getElementById('ark-toc-toggle');
    var closeBtn = document.getElementById('ark-toc-close');
    var listEl = document.getElementById('ark-toc-list');
    var filterEl = document.getElementById('ark-toc-filter');
    var countEl = document.getElementById('ark-toc-count');
    var moreBtn = document.getElementById('ark-toc-more');
    if (!panel || !toggleBtn || !listEl) return;

    var stem = panel.getAttribute('data-stem') || parseZIMPath().stem || '';
    if (!stem) return;
    var tocKey = 'ark_zim_toc_open_' + stem;
    var loadedOnce = false;
    var loading = false;
    var nextOffset = 0;
    var activeFilter = '';
    var origHtmlPadLeft = document.documentElement.style.paddingLeft || '';

    function isOpen() {
      return panel.style.display === 'flex';
    }

    function applyOpenState(open, persist) {
      panel.style.display = open ? 'flex' : 'none';
      toggleBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
      toggleBtn.style.background = open ? '#C25E28' : '#2A4338';
      if (open && window.innerWidth >= 900) {
        document.documentElement.style.setProperty('padding-left', '290px', 'important');
      } else if (origHtmlPadLeft) {
        document.documentElement.style.paddingLeft = origHtmlPadLeft;
      } else {
        document.documentElement.style.removeProperty('padding-left');
      }
      if (persist) {
        try {
          if (open) {
            localStorage.setItem(tocKey, '1');
          } else {
            localStorage.removeItem(tocKey);
          }
        } catch (e) {}
      }
      if (open && !loadedOnce) {
        loadedOnce = true;
        fetchPages(false);
      }
    }

    function renderPages(pages, append) {
      if (!append) listEl.innerHTML = '';
      var curPath = decodeURIComponent(location.pathname || '');
      if (!append && (!pages || pages.length === 0)) {
        var empty = document.createElement('div');
        empty.textContent = 'No pages found.';
        empty.style.cssText = 'padding:12px 10px;color:#9BB5A8;font-size:12px;';
        listEl.appendChild(empty);
        return;
      }
      var activeEl = null;
      for (var i = 0; i < pages.length; i++) {
        var p = pages[i];
        var a = document.createElement('a');
        a.href = p.url;
        a.textContent = p.title || p.url;
        a.title = p.title || p.url;
        var decodedHref = p.url;
        try { decodedHref = decodeURIComponent(p.url); } catch (e) {}
        var isCur = curPath === decodedHref || location.pathname === p.url;
        a.style.cssText = 'display:block;padding:6px 10px;border-radius:5px;text-decoration:none;font-size:13px;line-height:1.35;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;' +
          (isCur ? 'background:#C25E28;color:#FDFBF7;font-weight:600;' : 'color:#DCE8E1;');
        if (!isCur) {
          a.onmouseenter = function() { this.style.background = '#233A2F'; };
          a.onmouseleave = function() { this.style.background = 'transparent'; };
        } else {
          activeEl = a;
        }
        listEl.appendChild(a);
      }
      if (activeEl && !append) {
        setTimeout(function() { try { activeEl.scrollIntoView({ block: 'nearest' }); } catch (e) {} }, 20);
      }
    }

    function fetchPages(append) {
      if (loading) return;
      loading = true;
      var offset = append ? nextOffset : 0;
      if (!append && countEl) countEl.textContent = 'Loading...';
      if (moreBtn) moreBtn.style.display = 'none';
      var reqUrl = location.origin + '/api/zim/pages?stem=' + encodeURIComponent(stem) +
        '&offset=' + offset + '&limit=200' +
        (activeFilter ? '&q=' + encodeURIComponent(activeFilter) : '');
      fetch(reqUrl)
        .then(function(r) { return r.json(); })
        .then(function(d) {
          var pages = (d && d.pages) || [];
          renderPages(pages, append);
          nextOffset = (d && typeof d.next_offset === 'number') ? d.next_offset : (offset + pages.length);
          if (countEl) {
            var total = (d && typeof d.total === 'number') ? d.total : pages.length;
            countEl.textContent = total.toLocaleString() + (total === 1 ? ' page' : ' pages');
          }
          if (moreBtn) {
            moreBtn.style.display = (d && d.has_more) ? 'block' : 'none';
          }
        })
        .catch(function() {
          if (countEl) countEl.textContent = 'Error';
        })
        .finally(function() {
          loading = false;
        });
    }

    toggleBtn.addEventListener('click', function() {
      applyOpenState(!isOpen(), true);
    });
    if (closeBtn) {
      closeBtn.addEventListener('click', function() {
        applyOpenState(false, true);
      });
    }
    if (moreBtn) {
      moreBtn.addEventListener('click', function() {
        fetchPages(true);
      });
    }
    if (filterEl) {
      var filterTimer = null;
      filterEl.addEventListener('input', function() {
        clearTimeout(filterTimer);
        var val = (filterEl.value || '').trim();
        filterTimer = setTimeout(function() {
          activeFilter = val;
          nextOffset = 0;
          loadedOnce = true;
          fetchPages(false);
        }, 180);
      });
    }
    window.addEventListener('resize', function() {
      if (isOpen()) applyOpenState(true, false);
    });

    var savedOpen = false;
    try { savedOpen = localStorage.getItem(tocKey) === '1'; } catch (e) {}
    applyOpenState(savedOpen, false);
  })();
})();
</script>`
}

func buildReaderNavHTML(stem string, hideAddToAI bool) string {
	escStem := url.PathEscape(stem)
	safeStemAttr := html.EscapeString(stem)
	addBtn := ""
	if !hideAddToAI {
		addBtn = `<button type="button" onclick="var b=this;b.disabled=true;b.textContent='Adding...';var c=document.body.cloneNode(true);var n=c.querySelector('#ark-proxy-nav');if(n)n.remove();var txt=(c.innerText||'').trim().slice(0,800000);fetch(location.origin+'/api/ai/index-article',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({url:location.pathname,title:document.title,text:txt})}).then(async r=>{if(r.ok){var d=await r.json().catch(()=>({}));b.textContent='Added to AI'+(d.indexed_chunks?' ('+d.indexed_chunks+')':'');}else{var t=await r.text();b.textContent='Error';b.title=t;b.disabled=false;}}).catch(e=>{b.textContent='Error';b.title=e.message;b.disabled=false;});" style="background:#C25E28;color:#FDFBF7;border:none;border-radius:6px;padding:4px 12px;font-size:13px;font-weight:600;cursor:pointer;white-space:nowrap;">+ Add to AI</button>`
	}
	return `<nav id="ark-proxy-nav" style="position:fixed;top:0;left:0;right:0;width:100%;height:46px;max-height:46px;box-sizing:border-box;flex:none;align-self:flex-start;z-index:2147483647;background:#1B2E26;border-bottom:1px solid #2A4338;padding:6px 16px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;display:flex;align-items:center;gap:12px;overflow-x:auto;box-shadow:0 2px 6px rgba(27,46,38,.18);line-height:1.3;">` +
		`<button id="ark-toc-toggle" type="button" aria-expanded="false" aria-controls="ark-toc-panel" data-stem="` + safeStemAttr + `" style="display:inline-flex;align-items:center;gap:5px;background:#2A4338;color:#F5F2EB;border:1px solid #436153;border-radius:6px;padding:3px 10px;font-size:12px;font-weight:600;cursor:pointer;white-space:nowrap;">&#9776; Pages</button>` +
		`<a href="/" style="color:#F5F2EB;font-weight:700;text-decoration:none;font-size:14px;white-space:nowrap;">&#8592; Ark Home</a>` +
		`<span style="color:#436153;">|</span>` +
		`<a href="/library" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:13px;white-space:nowrap;">Library</a>` +
		`<a href="/read/zim/` + escStem + `/" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:13px;white-space:nowrap;">Main Page</a>` +
		`<a id="ark-ask-link" href="/ask" style="color:#C9DAD1;font-weight:500;text-decoration:none;font-size:13px;white-space:nowrap;">Ask AI</a>` +
		`<label style="display:inline-flex;align-items:center;gap:5px;color:#C9DAD1;font-size:12px;margin-left:auto;white-space:nowrap;">Theme <select id="ark-theme-sel" onchange="window.arkSetZIMTheme&&window.arkSetZIMTheme(this.value,true)" style="background:#12201A;color:#F5F2EB;border:1px solid #436153;border-radius:6px;padding:3px 8px;font-size:12px;cursor:pointer;"><option value="forest">Forest</option><option value="light">Light</option><option value="sepia">Sepia</option><option value="dark">Dark</option><option value="default">Original</option></select></label>` +
		`<form action="/read/zim/` + escStem + `/" method="GET" style="display:flex;gap:6px;margin:0;">` +
		`<input type="search" name="q" list="ark-zim-suggest-list" placeholder="Search ` + safeStemAttr + `..." oninput="clearTimeout(window._azt);window._azt=setTimeout(()=>{var v=this.value.trim();if(v.length<2)return;fetch('/api/zim/suggest?stem=` + escStem + `&q='+encodeURIComponent(v)).then(r=>r.json()).then(d=>{var dl=document.getElementById('ark-zim-suggest-list');if(!dl)return;dl.innerHTML='';(d.hits||[]).forEach(h=>{var o=document.createElement('option');o.value=h.title;dl.appendChild(o);});}).catch(()=>{});},180)" style="padding:4px 10px;border-radius:6px;border:1px solid #436153;background:#12201A;color:#F5F2EB;font-size:13px;min-width:170px;" required><datalist id="ark-zim-suggest-list"></datalist>` +
		`<button type="submit" style="background:#2A4338;color:#F5F2EB;border:1px solid #436153;border-radius:6px;padding:4px 10px;font-size:13px;cursor:pointer;">Search</button>` +
		`</form>` +
		`<a id="ark-download-html" href="?download=1" onclick="var u=new URL(location.href);u.searchParams.set('download','1');this.href=u.toString();" style="background:#2A4338;color:#F5F2EB;border:1px solid #436153;border-radius:6px;padding:4px 10px;font-size:13px;font-weight:600;text-decoration:none;white-space:nowrap;">&#8595; Download HTML</a>` +
		addBtn +
		`</nav>` +
		`<aside id="ark-toc-panel" data-stem="` + safeStemAttr + `" style="position:fixed;top:46px;left:0;bottom:0;width:290px;max-width:85vw;box-sizing:border-box;z-index:2147483646;background:#16261F;color:#F5F2EB;border-right:1px solid #2A4338;box-shadow:4px 0 18px rgba(0,0,0,.28);display:none;flex-direction:column;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">` +
		`<div style="display:flex;align-items:center;justify-content:space-between;gap:8px;padding:10px 12px;border-bottom:1px solid #2A4338;">` +
		`<div style="min-width:0;"><div style="font-size:13px;font-weight:700;color:#F5F2EB;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;">Table of Contents</div><div id="ark-toc-count" style="font-size:11px;color:#9BB5A8;">Collapsed</div></div>` +
		`<button id="ark-toc-close" type="button" title="Collapse" style="background:transparent;color:#C9DAD1;border:1px solid #436153;border-radius:6px;padding:2px 8px;font-size:14px;cursor:pointer;">&times;</button>` +
		`</div>` +
		`<div style="padding:8px 10px;border-bottom:1px solid #2A4338;"><input id="ark-toc-filter" type="search" placeholder="Filter pages..." style="width:100%;box-sizing:border-box;padding:6px 9px;border-radius:6px;border:1px solid #436153;background:#12201A;color:#F5F2EB;font-size:12px;"></div>` +
		`<div id="ark-toc-list" style="flex:1;overflow-y:auto;padding:6px;display:flex;flex-direction:column;gap:2px;"></div>` +
		`<button id="ark-toc-more" type="button" style="display:none;margin:8px 10px;padding:6px 10px;background:#2A4338;color:#F5F2EB;border:1px solid #436153;border-radius:6px;font-size:12px;font-weight:600;cursor:pointer;">Load more pages</button>` +
		`</aside>`
}

var (
	htmlTitleTagRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	htmlH1TagRe    = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	stripTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
)

func isSiteWideTitle(stem, candidate string) bool {
	c := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(candidate)), " ", "")
	s := strings.ToLower(strings.TrimSpace(stem))
	return c != "" && len(c) >= 4 && strings.Contains(s, c)
}

func formatSubpageLabel(stem string, entry zimResolvedEntry, htmlBytes []byte) string {
	if m := htmlH1TagRe.FindSubmatch(htmlBytes); len(m) > 1 {
		h1 := stripTagRe.ReplaceAllString(string(m[1]), " ")
		h1 = strings.Join(strings.Fields(html.UnescapeString(h1)), " ")
		if h1 != "" && !isSiteWideTitle(stem, h1) {
			return h1
		}
	}
	t := strings.TrimSpace(entry.title)
	if t != "" && t != entry.url && !strings.Contains(t, "/") && !isSiteWideTitle(stem, t) {
		return t
	}
	if m := htmlTitleTagRe.FindSubmatch(htmlBytes); len(m) > 1 {
		ht := stripTagRe.ReplaceAllString(string(m[1]), " ")
		ht = strings.Join(strings.Fields(html.UnescapeString(ht)), " ")
		if ht != "" && !strings.Contains(ht, "/") && !isSiteWideTitle(stem, ht) {
			return ht
		}
	}
	u := strings.TrimSuffix(strings.TrimSpace(entry.url), "/")
	if idx := strings.IndexAny(u, "?#"); idx != -1 {
		u = u[:idx]
	}
	parts := strings.Split(u, "/")
	seg := ""
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		if p == "" {
			continue
		}
		low := strings.ToLower(p)
		if (low == "index.html" || low == "index.htm" || low == "index.php" || low == "index") && i > 0 {
			continue
		}
		seg = p
		break
	}
	if seg == "" {
		seg = u
	}
	for _, ext := range []string{".html", ".htm", ".php", ".xhtml"} {
		if strings.HasSuffix(strings.ToLower(seg), ext) {
			seg = seg[:len(seg)-len(ext)]
			break
		}
	}
	if unesc, err := url.PathUnescape(seg); err == nil && unesc != "" {
		seg = unesc
	}
	seg = strings.ReplaceAll(seg, "_", " ")
	return strings.TrimSpace(seg)
}

func buildReaderPagerHTML(stem string, prev, next *zimTitleRecord, currentLabel string) string {
	escStem := url.PathEscape(stem)
	btnStyle := "display:inline-flex;align-items:center;justify-content:center;width:44px;height:44px;border-radius:999px;border:1px solid #436153;background:#1B2E26;color:#F5F2EB;font-size:20px;font-weight:700;text-decoration:none;box-shadow:0 2px 6px rgba(0,0,0,0.14);"
	disabledStyle := "display:inline-flex;align-items:center;justify-content:center;width:44px;height:44px;border-radius:999px;border:1px solid #2A4338;background:#1B2E26;color:#6E8579;font-size:20px;font-weight:700;opacity:0.4;cursor:default;"
	prevHTML := `<span aria-disabled="true" style="` + disabledStyle + `">&#8592;</span>`
	if prev != nil {
		href := fmt.Sprintf("/read/zim/%s/%c/%s", escStem, prev.ns, prev.url)
		prevHTML = fmt.Sprintf(`<a id="ark-prev-page" href="%s" aria-label="Previous page" style="%s">&#8592;</a>`,
			html.EscapeString(href), btnStyle)
	}
	nextHTML := `<span aria-disabled="true" style="` + disabledStyle + `">&#8594;</span>`
	if next != nil {
		href := fmt.Sprintf("/read/zim/%s/%c/%s", escStem, next.ns, next.url)
		nextHTML = fmt.Sprintf(`<a id="ark-next-page" href="%s" aria-label="Next page" style="%s">&#8594;</a>`,
			html.EscapeString(href), btnStyle)
	}
	labelHTML := ""
	if strings.TrimSpace(currentLabel) != "" {
		labelHTML = fmt.Sprintf(`<div id="ark-current-page-label" style="font-size:13px;font-weight:600;opacity:0.8;text-align:center;max-width:90vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">%s</div>`, html.EscapeString(currentLabel))
	}
	return `<div id="ark-page-pager" role="navigation" aria-label="Page sequence navigation" style="position:relative;left:auto;overflow:visible;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:8px;width:100%;margin:2rem auto 2.5rem;padding:0.75rem 1rem;box-sizing:border-box;clear:both;">` +
		`<div style="display:flex;align-items:center;justify-content:center;gap:20px;">` + prevHTML + nextHTML + `</div>` +
		labelHTML +
		`</div>`
}

func injectZIMReaderNav(htmlBytes []byte, stem string, archiveIndexed bool, prev, next *zimTitleRecord, currentLabel, initialTheme string) []byte {
	headSnippet := arkThemeHeadTag()
	navSnippet := buildReaderNavHTML(stem, archiveIndexed)
	pagerSnippet := buildReaderPagerHTML(stem, prev, next, currentLabel)
	tailSnippet := pagerSnippet + arkThemeScriptTag(initialTheme)

	out := make([]byte, 0, len(htmlBytes)+len(headSnippet)+len(navSnippet)+len(tailSnippet)+64)
	lower := bytes.ToLower(htmlBytes)

	headOpen := bytes.Index(lower, []byte("<head"))
	headClose := bytes.Index(lower, []byte("</head>"))
	bodyOpen := bytes.Index(lower, []byte("<body"))

	pos := 0
	if headOpen != -1 {
		if hEnd := bytes.IndexByte(lower[headOpen:], '>'); hEnd != -1 {
			insertHeadAt := headOpen + hEnd + 1
			out = append(out, htmlBytes[:insertHeadAt]...)
			out = append(out, []byte(`<script>window.__arkFetch=window.fetch;</script>`)...)
			pos = insertHeadAt
		}
	}
	if headClose != -1 && headClose >= pos {
		out = append(out, htmlBytes[pos:headClose]...)
		out = append(out, []byte(headSnippet)...)
		pos = headClose
	} else {
		out = append(out, []byte(headSnippet)...)
	}

	if bodyOpen != -1 && bodyOpen >= pos {
		closeIdx := bytes.IndexByte(lower[bodyOpen:], '>')
		if closeIdx != -1 {
			insertAt := bodyOpen + closeIdx + 1
			out = append(out, htmlBytes[pos:insertAt]...)
			out = append(out, []byte(navSnippet)...)
			rest := htmlBytes[insertAt:]
			if bodyClose := bytes.LastIndex(bytes.ToLower(rest), []byte("</body>")); bodyClose != -1 {
				out = append(out, rest[:bodyClose]...)
				out = append(out, []byte(tailSnippet)...)
				out = append(out, rest[bodyClose:]...)
			} else {
				out = append(out, rest...)
				out = append(out, []byte(tailSnippet)...)
			}
			return out
		}
	}

	out = append(out, []byte(navSnippet)...)
	out = append(out, htmlBytes[pos:]...)
	out = append(out, []byte(tailSnippet)...)
	return out
}

func (s *Server) renderLocalZIMIndex(w http.ResponseWriter) {
	stems := s.listLocalZIMStems()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var itemsHTML strings.Builder
	if len(stems) == 0 {
		itemsHTML.WriteString(`<p>No local ZIM archives found yet. <a href="/get-content">Download content packs</a> or <a href="/kiwix">browse the Kiwix Catalog</a>.</p>`)
	} else {
		itemsHTML.WriteString(`<ul style="list-style:none;padding:0;display:grid;gap:12px;">`)
		for _, stem := range stems {
			esc := html.EscapeString(stem)
			u := "/read/zim/" + url.PathEscape(stem) + "/"
			fmt.Fprintf(&itemsHTML, `<li style="background:#FDFBF7;border:1px solid #DCD6CC;border-radius:8px;padding:14px 16px;display:flex;justify-content:space-between;align-items:center;gap:12px;"><div><strong style="font-size:16px;">%s</strong></div><a href="%s" style="background:#C25E28;color:#FDFBF7;text-decoration:none;padding:6px 14px;border-radius:6px;font-weight:600;">Open &rarr;</a></li>`, esc, u)
		}
		itemsHTML.WriteString(`</ul>`)
	}
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Local ZIM Reader — Ark</title><link rel="stylesheet" href="/static/ark.css"></head><body style="padding:2rem;max-width:860px;margin:0 auto;"><p><a href="/library">&larr; Back to Library</a></p><h1>Local ZIM Reader</h1><p class="secondary-text">Reading directly from local ZIM archives stored on disk.</p>%s</body></html>`, itemsHTML.String())
}

func (s *Server) renderZIMSearchResults(w http.ResponseWriter, stem, query string, hits []zimTitleRecord, archiveIndexed bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	escStem := url.PathEscape(stem)
	var list strings.Builder
	if len(hits) == 0 {
		list.WriteString(`<p class="secondary-text">No matching article titles found.</p>`)
	} else {
		list.WriteString(`<ul style="line-height:1.9;">`)
		for _, h := range hits {
			href := fmt.Sprintf("/read/zim/%s/%c/%s", escStem, h.ns, h.url)
			fmt.Fprintf(&list, `<li><a href="%s">%s</a></li>`, html.EscapeString(href), html.EscapeString(h.title))
		}
		list.WriteString(`</ul>`)
	}
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Search: %s — %s</title><link rel="stylesheet" href="/static/ark.css">%s</head><body style="margin:0;">%s<main style="padding:2rem;max-width:860px;margin:0 auto;"><h1>Search in %s</h1><form action="/read/zim/%s/" method="GET" style="display:flex;gap:8px;margin-bottom:1.25rem;"><input type="search" name="q" value="%s" class="form-control" style="flex:1;"><button type="submit" class="btn btn-primary">Search</button></form><p class="secondary-text">%d result(s) for "%s"</p>%s</main>%s</body></html>`,
		html.EscapeString(query), html.EscapeString(stem), arkThemeHeadTag(), buildReaderNavHTML(stem, archiveIndexed), html.EscapeString(stem), escStem, html.EscapeString(query), len(hits), html.EscapeString(query), list.String(), arkThemeScriptTag(s.getSavedReaderTheme()))
}

var (
	linkStylesheetRe = regexp.MustCompile(`(?is)<link\b[^>]*rel=["']?[^"'>]*stylesheet[^"'>]*["']?[^>]*>`)
	hrefAttrRe       = regexp.MustCompile(`(?is)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	srcAttrRe        = regexp.MustCompile(`(?is)\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	posterAttrRe     = regexp.MustCompile(`(?is)\bposter\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	srcsetAttrRe     = regexp.MustCompile(`(?is)\s+\bsrcset\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
	scriptSrcRe      = regexp.MustCompile(`(?is)<script\b([^>]*)\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))([^>]*)>\s*</script>`)
	imgOrMediaTagRe  = regexp.MustCompile(`(?is)<(?:img|source|video|audio)\b[^>]*>`)
	cssURLRe         = regexp.MustCompile(`(?is)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s]+))\s*\)`)
)

func sanitizeDownloadHTMLFilename(label, fallbackURL string) string {
	name := strings.TrimSpace(label)
	if name == "" {
		name = path.Base(strings.TrimSuffix(fallbackURL, "/"))
	}
	if name == "" || name == "." || name == "/" {
		name = "page"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	clean := strings.TrimSpace(b.String())
	if clean == "" {
		clean = "page"
	}
	if !strings.HasSuffix(strings.ToLower(clean), ".html") {
		clean += ".html"
	}
	return clean
}

func resolveZIMRelativeAsset(arc *zimArchive, f *os.File, baseNS byte, baseURL, rawRef string) ([]byte, string, bool) {
	ref := strings.TrimSpace(rawRef)
	if ref == "" {
		return nil, "", false
	}
	low := strings.ToLower(ref)
	if strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "mailto:") || strings.HasPrefix(low, "#") || strings.HasPrefix(ref, "//") {
		return nil, "", false
	}
	if idx := strings.IndexAny(ref, "?#"); idx != -1 {
		ref = ref[:idx]
	}
	if unesc, err := url.PathUnescape(ref); err == nil && unesc != "" {
		ref = unesc
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, "", false
	}

	var candidates []string
	if strings.HasPrefix(ref, "/") {
		candidates = append(candidates, strings.TrimPrefix(ref, "/"))
	} else {
		dir := path.Dir(strings.TrimPrefix(baseURL, "/"))
		if dir == "." {
			candidates = append(candidates, path.Clean(ref))
		} else {
			candidates = append(candidates, path.Clean(dir+"/"+ref))
		}
		candidates = append(candidates, strings.TrimPrefix(ref, "./"))
	}

	for _, cand := range candidates {
		cand = strings.TrimPrefix(cand, "/")
		if cand == "" || cand == "." {
			continue
		}
		var entry zimResolvedEntry
		var ok bool
		if baseNS != 0 {
			entry, ok = arc.findEntry(f, baseNS, cand)
		}
		if !ok {
			entry, ok = arc.findEntryFlex(f, cand)
		}
		if !ok {
			continue
		}
		if entry.mimeID == 0xffff {
			if redir, err := arc.followRedirects(f, entry); err == nil {
				entry = redir
			}
		}
		if entry.mimeID >= 0xfffd {
			continue
		}
		blob, err := arc.readBlob(f, entry.clusterNum, entry.blobNum)
		if err != nil {
			continue
		}
		mimeType := "application/octet-stream"
		if int(entry.mimeID) < len(arc.mimes) && arc.mimes[entry.mimeID] != "" {
			mimeType = arc.mimes[entry.mimeID]
		}
		return blob, mimeType, true
	}
	return nil, "", false
}

func inlineZIMCSSAssets(arc *zimArchive, f *os.File, cssNS byte, cssURL string, cssBytes []byte) []byte {
	return cssURLRe.ReplaceAllFunc(cssBytes, func(match []byte) []byte {
		sub := cssURLRe.FindSubmatch(match)
		if len(sub) < 4 {
			return match
		}
		ref := string(sub[1])
		if ref == "" {
			ref = string(sub[2])
		}
		if ref == "" {
			ref = string(sub[3])
		}
		blob, mimeType, ok := resolveZIMRelativeAsset(arc, f, cssNS, cssURL, ref)
		if !ok || len(blob) > 4*1024*1024 {
			return match
		}
		return []byte(`url("data:` + mimeType + `;base64,` + base64.StdEncoding.EncodeToString(blob) + `")`)
	})
}

// inlineZIMPageAssets embeds local CSS stylesheets, JS scripts, and media/image assets
// into a standalone HTML document so downloading a page preserves full styling and functionality offline.
func inlineZIMPageAssets(arc *zimArchive, f *os.File, entry zimResolvedEntry, rawHTML []byte) []byte {
	out := string(rawHTML)

	// 1. Inline <link rel="stylesheet" href="...">
	out = linkStylesheetRe.ReplaceAllStringFunc(out, func(tag string) string {
		m := hrefAttrRe.FindStringSubmatch(tag)
		if len(m) < 4 {
			return tag
		}
		href := m[1]
		if href == "" {
			href = m[2]
		}
		if href == "" {
			href = m[3]
		}
		blob, _, ok := resolveZIMRelativeAsset(arc, f, entry.ns, entry.url, href)
		if !ok {
			return tag
		}
		inlinedCSS := inlineZIMCSSAssets(arc, f, entry.ns, path.Clean(path.Dir(entry.url)+"/"+href), blob)
		safeCSS := strings.ReplaceAll(string(inlinedCSS), "</style", `<\/style`)
		return "<style>\n" + safeCSS + "\n</style>"
	})

	// 2. Inline <script src="..."></script>
	out = scriptSrcRe.ReplaceAllStringFunc(out, func(tag string) string {
		m := scriptSrcRe.FindStringSubmatch(tag)
		if len(m) < 6 {
			return tag
		}
		src := m[2]
		if src == "" {
			src = m[3]
		}
		if src == "" {
			src = m[4]
		}
		blob, _, ok := resolveZIMRelativeAsset(arc, f, entry.ns, entry.url, src)
		if !ok || len(blob) > 4*1024*1024 {
			return tag
		}
		safeJS := strings.ReplaceAll(string(blob), "</script", `<\/script`)
		return "<script" + m[1] + m[5] + ">\n" + safeJS + "\n</script>"
	})

	// 3. Inline <img src="...">, <source src="...">, <video poster="...">
	out = imgOrMediaTagRe.ReplaceAllStringFunc(out, func(tag string) string {
		updated := tag
		if m := srcAttrRe.FindStringSubmatch(updated); len(m) >= 4 {
			src := m[1]
			if src == "" {
				src = m[2]
			}
			if src == "" {
				src = m[3]
			}
			if blob, mimeType, ok := resolveZIMRelativeAsset(arc, f, entry.ns, entry.url, src); ok && len(blob) <= 12*1024*1024 {
				dataURI := `src="data:` + mimeType + `;base64,` + base64.StdEncoding.EncodeToString(blob) + `"`
				updated = srcAttrRe.ReplaceAllString(updated, dataURI)
				updated = srcsetAttrRe.ReplaceAllString(updated, "")
			}
		}
		if m := posterAttrRe.FindStringSubmatch(updated); len(m) >= 4 {
			poster := m[1]
			if poster == "" {
				poster = m[2]
			}
			if poster == "" {
				poster = m[3]
			}
			if blob, mimeType, ok := resolveZIMRelativeAsset(arc, f, entry.ns, entry.url, poster); ok && len(blob) <= 6*1024*1024 {
				dataURI := `poster="data:` + mimeType + `;base64,` + base64.StdEncoding.EncodeToString(blob) + `"`
				updated = posterAttrRe.ReplaceAllString(updated, dataURI)
			}
		}
		return updated
	})

	return []byte(out)
}
