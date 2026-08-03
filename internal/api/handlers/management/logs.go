package management

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

const (
	defaultLogFileName      = "main.log"
	logScannerInitialBuffer = 64 * 1024
	logScannerMaxBuffer     = 8 * 1024 * 1024
)

// logReadCache stores only per-file counters and the byte cursor for the
// incomplete tail. It deliberately does not retain log bodies in memory: the
// common polling path can skip immutable files while keeping memory bounded.
type logReadCache struct {
	mu    sync.Mutex
	dir   string
	files map[string]logFileCacheEntry
}

type logFileCacheEntry struct {
	size    int64
	modTime int64

	// completeSize is the byte offset immediately after the last complete
	// newline that was consumed. tailBytes starts at this offset.
	completeSize          int64
	completeTotal         int
	completeLatest        int64
	completeLastTimestamp int64

	total         int
	latest        int64
	lastTimestamp int64
	tailBytes     int64
}

func (c *logReadCache) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.dir = ""
	c.files = nil
	c.mu.Unlock()
}

func (h *Handler) resetLogReadCache() {
	if h != nil {
		h.logReadCache.reset()
	}
}

// GetLogs returns log lines with optional incremental loading.
func (h *Handler) GetLogs(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	if !h.cfg.LoggingToFile {
		c.JSON(http.StatusBadRequest, gin.H{"error": "logging to file disabled"})
		return
	}

	logDir := h.logDirectory()
	if strings.TrimSpace(logDir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}

	files, err := h.collectLogFiles(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			cutoff := parseCutoff(c.Query("after"))
			c.JSON(http.StatusOK, gin.H{
				"lines":            []string{},
				"line-count":       0,
				"latest-timestamp": cutoff,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to list log files: %v", err)})
		return
	}

	limit, errLimit := parseLimit(c.Query("limit"))
	if errLimit != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid limit: %v", errLimit)})
		return
	}

	cutoff := parseCutoff(c.Query("after"))
	lines, total, latest, errRead := h.readLogsIncrementally(logDir, files, cutoff, limit)
	if errRead != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRead.Error()})
		return
	}
	if latest == 0 || latest < cutoff {
		latest = cutoff
	}
	c.JSON(http.StatusOK, gin.H{
		"lines":            lines,
		"line-count":       total,
		"latest-timestamp": latest,
	})
}

// DeleteLogs removes all rotated log files and truncates the active log.
func (h *Handler) DeleteLogs(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	if !h.cfg.LoggingToFile {
		c.JSON(http.StatusBadRequest, gin.H{"error": "logging to file disabled"})
		return
	}

	dir := h.logDirectory()
	if strings.TrimSpace(dir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "log directory not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to list log directory: %v", err)})
		return
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		fullPath := filepath.Join(dir, name)
		if name == defaultLogFileName {
			if errTrunc := os.Truncate(fullPath, 0); errTrunc != nil && !os.IsNotExist(errTrunc) {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to truncate log file: %v", errTrunc)})
				return
			}
			continue
		}
		if isRotatedLogFile(name) {
			if errRemove := os.Remove(fullPath); errRemove != nil && !os.IsNotExist(errRemove) {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to remove %s: %v", name, errRemove)})
				return
			}
			removed++
		}
	}
	h.resetLogReadCache()

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Logs cleared successfully",
		"removed": removed,
	})
}

// GetRequestErrorLogs lists error request log files when RequestLog is disabled.
// It returns an empty list when RequestLog is enabled.
func (h *Handler) GetRequestErrorLogs(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	if h.cfg.RequestLog {
		c.JSON(http.StatusOK, gin.H{"files": []any{}})
		return
	}

	dir := h.logDirectory()
	if strings.TrimSpace(dir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, gin.H{"files": []any{}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to list request error logs: %v", err)})
		return
	}

	type errorLog struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		Modified int64  `json:"modified"`
	}

	files := make([]errorLog, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "error-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read log info for %s: %v", name, errInfo)})
			return
		}
		files = append(files, errorLog{
			Name:     name,
			Size:     info.Size(),
			Modified: info.ModTime().Unix(),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Modified > files[j].Modified })

	c.JSON(http.StatusOK, gin.H{"files": files})
}

// GetRequestLogByID finds and downloads a request log file by its request ID.
// The ID is matched against the suffix of log file names (format: *-{requestID}.log).
func (h *Handler) GetRequestLogByID(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}

	dir := h.logDirectory()
	if strings.TrimSpace(dir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}

	requestID := strings.TrimSpace(c.Param("id"))
	if requestID == "" {
		requestID = strings.TrimSpace(c.Query("id"))
	}
	if requestID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing request ID"})
		return
	}
	if strings.ContainsAny(requestID, "/\\") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request ID"})
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "log directory not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to list log directory: %v", err)})
		return
	}

	suffix := "-" + requestID + ".log"
	var matchedFile string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, suffix) {
			matchedFile = name
			break
		}
	}

	if matchedFile == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "log file not found for the given request ID"})
		return
	}

	dirAbs, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to resolve log directory: %v", errAbs)})
		return
	}
	fullPath := filepath.Clean(filepath.Join(dirAbs, matchedFile))
	prefix := dirAbs + string(os.PathSeparator)
	if !strings.HasPrefix(fullPath, prefix) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log file path"})
		return
	}

	info, errStat := os.Stat(fullPath)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			c.JSON(http.StatusNotFound, gin.H{"error": "log file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read log file: %v", errStat)})
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log file"})
		return
	}

	c.FileAttachment(fullPath, matchedFile)
}

// DownloadRequestErrorLog downloads a specific error request log file by name.
func (h *Handler) DownloadRequestErrorLog(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}

	dir := h.logDirectory()
	if strings.TrimSpace(dir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}

	name := strings.TrimSpace(c.Param("name"))
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log file name"})
		return
	}
	if !strings.HasPrefix(name, "error-") || !strings.HasSuffix(name, ".log") {
		c.JSON(http.StatusNotFound, gin.H{"error": "log file not found"})
		return
	}

	dirAbs, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to resolve log directory: %v", errAbs)})
		return
	}
	fullPath := filepath.Clean(filepath.Join(dirAbs, name))
	prefix := dirAbs + string(os.PathSeparator)
	if !strings.HasPrefix(fullPath, prefix) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log file path"})
		return
	}

	info, errStat := os.Stat(fullPath)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			c.JSON(http.StatusNotFound, gin.H{"error": "log file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read log file: %v", errStat)})
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log file"})
		return
	}

	c.FileAttachment(fullPath, name)
}

func (h *Handler) logDirectory() string {
	if h == nil {
		return ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.logDir != "" {
		return h.logDir
	}
	return logging.ResolveLogDirectory(h.cfg)
}

func (h *Handler) collectLogFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		path  string
		order int64
	}
	cands := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == defaultLogFileName {
			cands = append(cands, candidate{path: filepath.Join(dir, name), order: 0})
			continue
		}
		if order, ok := rotationOrder(name); ok {
			cands = append(cands, candidate{path: filepath.Join(dir, name), order: order})
		}
	}
	if len(cands) == 0 {
		return []string{}, nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].order < cands[j].order })
	paths := make([]string, 0, len(cands))
	for i := len(cands) - 1; i >= 0; i-- {
		paths = append(paths, cands[i].path)
	}
	return paths, nil
}

// readLogsIncrementally serves the usual after>0 polling request from a
// per-file cursor. When a cursor is unavailable, a file is rewritten, or a
// caller asks for an older cutoff than the cache can reconstruct, it falls
// back to the original full scan and refreshes the cache.
func (h *Handler) readLogsIncrementally(dir string, files []string, cutoff int64, limit int) ([]string, int, int64, error) {
	if h == nil {
		return nil, 0, 0, fmt.Errorf("handler unavailable")
	}

	h.logReadCache.mu.Lock()
	defer h.logReadCache.mu.Unlock()

	if cutoff <= 0 || h.logReadCache.dir != dir || h.logReadCache.files == nil {
		return h.rebuildLogReadCacheLocked(dir, files, cutoff, limit)
	}

	acc := newLogAccumulator(cutoff, limit)
	updated := make(map[string]logFileCacheEntry, len(files))
	for _, path := range files {
		info, errStat := os.Stat(path)
		if errStat != nil {
			if os.IsNotExist(errStat) {
				return h.rebuildLogReadCacheLocked(dir, files, cutoff, limit)
			}
			return nil, 0, 0, fmt.Errorf("failed to stat log file %s: %w", path, errStat)
		}

		cached, ok := h.logReadCache.files[path]
		if !ok {
			return h.rebuildLogReadCacheLocked(dir, files, cutoff, limit)
		}
		if cached.latest > cutoff {
			// The cache stores counters rather than line bodies, so an older
			// cutoff cannot be reconstructed without scanning the file.
			return h.rebuildLogReadCacheLocked(dir, files, cutoff, limit)
		}

		if info.Size() == cached.size && info.ModTime().UnixNano() == cached.modTime {
			acc.addCachedLogSummary(cached, cutoff)
			updated[path] = cached
			continue
		}

		// main.log is append-only between rotations. Re-read only the old
		// incomplete tail and bytes appended after the cached cursor.
		if filepath.Base(path) != defaultLogFileName || info.Size() < cached.size {
			return h.rebuildLogReadCacheLocked(dir, files, cutoff, limit)
		}
		complete := logFileCacheEntry{
			completeTotal:         cached.completeTotal,
			completeLatest:        cached.completeLatest,
			completeLastTimestamp: cached.completeLastTimestamp,
			total:                 cached.completeTotal,
			latest:                cached.completeLatest,
			lastTimestamp:         cached.completeLastTimestamp,
		}
		acc.addCachedLogSummary(complete, cutoff)
		refreshed, errScan := scanLogFile(path, cached.completeSize, acc, complete)
		if errScan != nil {
			return nil, 0, 0, fmt.Errorf("failed to read log file %s: %w", path, errScan)
		}
		refreshed.modTime = info.ModTime().UnixNano()
		updated[path] = refreshed
	}

	h.logReadCache.dir = dir
	h.logReadCache.files = updated
	lines, total, latest := acc.result()
	return lines, total, latest, nil
}

func (h *Handler) rebuildLogReadCacheLocked(dir string, files []string, cutoff int64, limit int) ([]string, int, int64, error) {
	acc := newLogAccumulator(cutoff, limit)
	updated := make(map[string]logFileCacheEntry, len(files))
	for _, path := range files {
		info, errStat := os.Stat(path)
		if errStat != nil {
			if os.IsNotExist(errStat) {
				continue
			}
			return nil, 0, 0, fmt.Errorf("failed to stat log file %s: %w", path, errStat)
		}
		entry, errScan := scanLogFile(path, 0, acc, logFileCacheEntry{})
		if errScan != nil {
			return nil, 0, 0, fmt.Errorf("failed to read log file %s: %w", path, errScan)
		}
		entry.size = entry.completeSize + entry.tailBytes
		entry.modTime = info.ModTime().UnixNano()
		updated[path] = entry
	}
	h.logReadCache.dir = dir
	h.logReadCache.files = updated
	lines, total, latest := acc.result()
	return lines, total, latest, nil
}

func (acc *logAccumulator) addCachedLogSummary(file logFileCacheEntry, cutoff int64) {
	acc.total += file.total
	if file.latest > acc.latest {
		acc.latest = file.latest
	}
	if file.lastTimestamp > 0 {
		acc.include = cutoff == 0 || file.lastTimestamp > cutoff
	}
}

// scanLogFile consumes complete lines and, when present, the final partial
// line. Starting at completeSize lets an append-only main.log reuse its old
// summary while still handling a line that was incomplete during the last poll.
func scanLogFile(path string, completeSize int64, acc *logAccumulator, base logFileCacheEntry) (logFileCacheEntry, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		if os.IsNotExist(errOpen) {
			return logFileCacheEntry{}, nil
		}
		return logFileCacheEntry{}, errOpen
	}
	defer func() { _ = file.Close() }()

	if completeSize > 0 {
		if _, errSeek := file.Seek(completeSize, io.SeekStart); errSeek != nil {
			return logFileCacheEntry{}, errSeek
		}
	}

	entry := logFileCacheEntry{
		completeSize:          completeSize,
		completeTotal:         base.completeTotal,
		completeLatest:        base.completeLatest,
		completeLastTimestamp: base.completeLastTimestamp,
		latest:                base.completeLatest,
		lastTimestamp:         base.completeLastTimestamp,
	}
	reader := bufio.NewReaderSize(file, logScannerInitialBuffer)
	for {
		raw, errRead := reader.ReadString('\n')
		if len(raw) == 0 && errRead == io.EOF {
			break
		}
		if len(raw) > logScannerMaxBuffer+1 {
			return logFileCacheEntry{}, fmt.Errorf("bufio.Scanner: token too long")
		}
		if errRead != nil && errRead != io.EOF {
			return logFileCacheEntry{}, errRead
		}

		complete := errRead == nil
		rawLen := len(raw)
		if complete {
			raw = strings.TrimSuffix(raw, "\n")
		}
		line := strings.TrimRight(raw, "\r")
		ts := parseTimestamp(line)
		acc.addLineWithTimestamp(line, ts)
		if ts > entry.latest {
			entry.latest = ts
		}
		if ts > 0 {
			entry.lastTimestamp = ts
		}

		if complete {
			entry.completeSize += int64(rawLen)
			entry.completeTotal++
			if ts > entry.completeLatest {
				entry.completeLatest = ts
			}
			if ts > 0 {
				entry.completeLastTimestamp = ts
			}
			entry.tailBytes = 0
		} else {
			entry.tailBytes = int64(rawLen)
			break
		}
		if errRead == io.EOF {
			break
		}
	}
	entry.total = entry.completeTotal
	if entry.tailBytes > 0 {
		entry.total++
	}
	entry.size = entry.completeSize + entry.tailBytes
	return entry, nil
}

type logAccumulator struct {
	cutoff  int64
	limit   int
	lines   []string
	total   int
	latest  int64
	include bool
}

func newLogAccumulator(cutoff int64, limit int) *logAccumulator {
	capacity := 256
	if limit > 0 && limit < capacity {
		capacity = limit
	}
	return &logAccumulator{
		cutoff: cutoff,
		limit:  limit,
		lines:  make([]string, 0, capacity),
	}
}

func (acc *logAccumulator) consumeFile(path string) error {
	// #nosec G304 -- paths come from collectLogFiles, which only returns known log filenames from the configured log directory.
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() {
		_ = file.Close()
	}()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, logScannerInitialBuffer)
	scanner.Buffer(buf, logScannerMaxBuffer)
	for scanner.Scan() {
		acc.addLine(scanner.Text())
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	return nil
}

func (acc *logAccumulator) addLine(raw string) {
	acc.addLineWithTimestamp(raw, parseTimestamp(raw))
}

func (acc *logAccumulator) addLineWithTimestamp(raw string, ts int64) {
	line := strings.TrimRight(raw, "\r")
	acc.total++
	if ts > acc.latest {
		acc.latest = ts
	}
	if ts > 0 {
		acc.include = acc.cutoff == 0 || ts > acc.cutoff
		if acc.cutoff == 0 || acc.include {
			acc.append(line)
		}
		return
	}
	if acc.cutoff == 0 || acc.include {
		acc.append(line)
	}
}

func (acc *logAccumulator) append(line string) {
	acc.lines = append(acc.lines, line)
	if acc.limit > 0 && len(acc.lines) > acc.limit {
		acc.lines = acc.lines[len(acc.lines)-acc.limit:]
	}
}

func (acc *logAccumulator) result() ([]string, int, int64) {
	if acc.lines == nil {
		acc.lines = []string{}
	}
	return acc.lines, acc.total, acc.latest
}

func parseCutoff(raw string) int64 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}
	ts, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ts <= 0 {
		return 0
	}
	return ts
}

func parseLimit(raw string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("must be a positive integer")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("must be greater than zero")
	}
	return limit, nil
}

func parseTimestamp(line string) int64 {
	line = strings.TrimPrefix(line, "[")
	if len(line) < 19 {
		return 0
	}
	if line[4] != '-' || line[7] != '-' || line[10] != ' ' || line[13] != ':' || line[16] != ':' {
		return 0
	}
	values := [6]int{}
	positions := [...]int{0, 5, 8, 11, 14, 17}
	limits := [...]int{9999, 12, 31, 23, 59, 59}
	for i, pos := range positions {
		if line[pos] < '0' || line[pos] > '9' || line[pos+1] < '0' || line[pos+1] > '9' {
			return 0
		}
		value := int(line[pos]-'0')*10 + int(line[pos+1]-'0')
		if i == 0 {
			if line[pos+2] < '0' || line[pos+2] > '9' || line[pos+3] < '0' || line[pos+3] > '9' {
				return 0
			}
			value = value*100 + int(line[pos+2]-'0')*10 + int(line[pos+3]-'0')
		}
		if (i < 3 && value <= 0) || value > limits[i] {
			return 0
		}
		values[i] = value
	}
	if values[1] < 1 || values[2] < 1 {
		return 0
	}
	t := time.Date(values[0], time.Month(values[1]), values[2], values[3], values[4], values[5], 0, time.Local)
	if t.Year() != values[0] || int(t.Month()) != values[1] || t.Day() != values[2] || t.Hour() != values[3] || t.Minute() != values[4] || t.Second() != values[5] {
		return 0
	}
	return t.Unix()
}

func isRotatedLogFile(name string) bool {
	if _, ok := rotationOrder(name); ok {
		return true
	}
	return false
}

func rotationOrder(name string) (int64, bool) {
	if order, ok := numericRotationOrder(name); ok {
		return order, true
	}
	if order, ok := timestampRotationOrder(name); ok {
		return order, true
	}
	return 0, false
}

func numericRotationOrder(name string) (int64, bool) {
	if !strings.HasPrefix(name, defaultLogFileName+".") {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, defaultLogFileName+".")
	if suffix == "" {
		return 0, false
	}
	n, err := strconv.Atoi(suffix)
	if err != nil {
		return 0, false
	}
	return int64(n), true
}

func timestampRotationOrder(name string) (int64, bool) {
	ext := filepath.Ext(defaultLogFileName)
	base := strings.TrimSuffix(defaultLogFileName, ext)
	if base == "" {
		return 0, false
	}
	prefix := base + "-"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	clean := strings.TrimPrefix(name, prefix)
	clean = strings.TrimSuffix(clean, ".gz")
	if ext != "" {
		if !strings.HasSuffix(clean, ext) {
			return 0, false
		}
		clean = strings.TrimSuffix(clean, ext)
	}
	if clean == "" {
		return 0, false
	}
	if idx := strings.IndexByte(clean, '.'); idx != -1 {
		clean = clean[:idx]
	}
	parsed, err := time.ParseInLocation("2006-01-02T15-04-05", clean, time.Local)
	if err != nil {
		return 0, false
	}
	return math.MaxInt64 - parsed.Unix(), true
}
